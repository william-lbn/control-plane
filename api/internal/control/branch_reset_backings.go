package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"regexp"
)

var branchResetOperationID = regexp.MustCompile(`^op_[a-f0-9]{24}$`)
var errBranchResetBacking = errors.New("original branch reset private backing changed")

// This complete private snapshot lives only in an immutable Kubernetes Secret.
// Neither raw config bytes nor credentials enter metadata, Operation or logs.
type branchResetBacking struct {
	Operation      string                  `json:"operation"`
	Original       computeCatalogBootstrap `json:"original"`
	Target         catalogNativeScope      `json:"target"`
	ConfigData     map[string]string       `json:"config_data"`
	CredentialData map[string]string       `json:"credential_data"`
	PreparedData   map[string]string       `json:"prepared_data"`
}

func resetSecretData(secret map[string]any) (map[string]string, error) {
	raw, ok := secret["data"].(map[string]any)
	if !ok || len(raw) == 0 || len(raw) > 64 {
		return nil, errBranchResetBacking
	}
	result := map[string]string{}
	total := 0
	for key, value := range raw {
		text, ok := value.(string)
		if !ok || key == "" {
			return nil, errBranchResetBacking
		}
		decoded, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return nil, errBranchResetBacking
		}
		total += len(decoded)
		if total > 512<<10 {
			return nil, errBranchResetBacking
		}
		result[key] = text
	}
	return result, nil
}

func prepareBranchResetBacking(operation string, original computeCatalogBootstrap, target catalogNativeScope, config, credential map[string]any) (branchResetBacking, error) {
	value := branchResetBacking{Operation: operation, Original: original, Target: target}
	if !branchResetOperationID.MatchString(operation) || !functionWriterEndpointID.MatchString(original.Endpoint) ||
		!functionResourceUID.MatchString(original.ConfigUID) || !functionResourceUID.MatchString(original.CredentialUID) ||
		!owned(config, original.Project, original.Endpoint) || !owned(credential, original.Project, original.Endpoint) ||
		nested(config, "metadata", "uid") != original.ConfigUID || nested(credential, "metadata", "uid") != original.CredentialUID {
		return value, errBranchResetBacking
	}
	var err error
	if value.ConfigData, err = resetSecretData(config); err != nil {
		return value, err
	}
	if value.CredentialData, err = resetSecretData(credential); err != nil {
		return value, err
	}
	raw, err := secretText(config, "config.json")
	if err != nil {
		return value, errBranchResetBacking
	}
	var source map[string]any
	if json.Unmarshal([]byte(raw), &source) != nil {
		return value, errBranchResetBacking
	}
	prepared, err := branchResetComputeConfig(source, original, target)
	if err != nil {
		return value, err
	}
	// The original files must agree before a rollback snapshot can be trusted.
	probe, err := secretText(config, "probePassword")
	password, credentialErr := secretText(credential, "probePassword")
	admin, adminErr := secretText(credential, "adminVerifier")
	verifier, verifierErr := secretText(credential, "probeVerifier")
	if err != nil || credentialErr != nil || adminErr != nil || verifierErr != nil || probe == "" || probe != password || !scramMatches(probe, verifier) {
		return value, errBranchResetBacking
	}
	roots := map[string]string{}
	for _, raw := range nested(prepared, "spec", "cluster", "roles").([]any) {
		role := raw.(map[string]any)
		roots[stringVal(role["name"])] = stringVal(role["encrypted_password"])
	}
	if roots["cloud_admin"] != admin || roots["control_probe"] != verifier {
		return value, errBranchResetBacking
	}
	encoded, err := json.Marshal(prepared)
	if err != nil {
		return value, err
	}
	value.PreparedData = map[string]string{}
	for key, text := range value.ConfigData {
		value.PreparedData[key] = text
	}
	value.PreparedData["config.json"] = base64.StdEncoding.EncodeToString(encoded)
	return value, nil
}

func (v branchResetBacking) secretName() string {
	return "branch-reset-" + catalogMarkerDigest(v.Operation + "/" + v.Original.Endpoint)[:40]
}
func (v branchResetBacking) digest() string {
	raw, _ := json.Marshal(v)
	return catalogMarkerDigest(string(raw))
}
func (v branchResetBacking) preparedDigest() string {
	raw, _ := json.Marshal(v.PreparedData)
	return catalogMarkerDigest(string(raw))
}

func validateBranchResetBacking(value branchResetBacking) error {
	project, endpoint := value.Original.Project, value.Original.Endpoint
	config := map[string]any{"metadata": map[string]any{"uid": value.Original.ConfigUID, "labels": resetDataAny(credentialLabels(project, endpoint))}, "data": resetDataAny(value.ConfigData)}
	credential := map[string]any{"metadata": map[string]any{"uid": value.Original.CredentialUID, "labels": resetDataAny(credentialLabels(project, endpoint))}, "data": resetDataAny(value.CredentialData)}
	checked, err := prepareBranchResetBacking(value.Operation, value.Original, value.Target, config, credential)
	if err != nil || checked.digest() != value.digest() {
		return errBranchResetBacking
	}
	return nil
}
func (k *kubeClient) reserveBranchResetBacking(ctx context.Context, value branchResetBacking) (string, string, error) {
	if err := validateBranchResetBacking(value); err != nil {
		return "", "", err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", "", err
	}
	labels := credentialLabels(value.Original.Project, value.Original.Endpoint)
	labels["neon-control/reset-operation"] = value.Operation
	data := map[string]string{"snapshot.json": base64.StdEncoding.EncodeToString(raw)}
	secret, err := k.createOwned(ctx, "secret", map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "immutable": true,
		"metadata": map[string]any{"name": value.secretName(), "labels": labels}, "data": data}, value.Original.Project, value.Original.Endpoint)
	if err != nil {
		originalErr := err
		var fault kubeError
		if errors.As(err, &fault) && fault.Status < 500 {
			return "", "", err
		}
		secret, err = k.request(ctx, http.MethodGet, k.path("secret", value.secretName()), nil)
		if err != nil {
			return "", "", originalErr
		}
	}
	stored, err := resetSecretData(secret)
	uid := stringVal(nested(secret, "metadata", "uid"))
	if err != nil || !reflect.DeepEqual(stored, data) || secret["immutable"] != true || !functionResourceUID.MatchString(uid) ||
		nested(secret, "metadata", "labels", "neon-control/reset-operation") != value.Operation {
		return "", "", errBranchResetBacking
	}
	return value.secretName(), uid, nil
}

func (k *kubeClient) readBranchResetBacking(ctx context.Context, operation, project, endpoint, ref, uid, digest string) (branchResetBacking, error) {
	var value branchResetBacking
	secret, err := k.request(ctx, http.MethodGet, k.path("secret", ref), nil)
	if err != nil {
		return value, err
	}
	if !owned(secret, project, endpoint) || nested(secret, "metadata", "uid") != uid || secret["immutable"] != true ||
		nested(secret, "metadata", "labels", "neon-control/reset-operation") != operation {
		return value, errBranchResetBacking
	}
	raw, err := secretText(secret, "snapshot.json")
	if err != nil || json.Unmarshal([]byte(raw), &value) != nil || value.Operation != operation || value.Original.Project != project || value.Original.Endpoint != endpoint ||
		value.secretName() != ref || value.digest() != digest {
		return value, errBranchResetBacking
	}
	// Reconstruct from the original complete files, rather than trusting a forged
	// prepared projection in a retained document with superficially valid labels.
	if err = validateBranchResetBacking(value); err != nil {
		return value, err
	}
	return value, nil
}
func resetDataAny(values map[string]string) map[string]any {
	result := map[string]any{}
	for key, value := range values {
		result[key] = value
	}
	return result
}

// Apply and rollback are both fixed-UID CAS transitions between the two exact
// retained documents. Unknown writes are recovered by observing the same UID;
// a third state, replacement Secret or credential drift is never overwritten.
func (k *kubeClient) transitionBranchResetBacking(ctx context.Context, value branchResetBacking, rollback bool) error {
	if err := validateBranchResetBacking(value); err != nil {
		return err
	}
	want, before := value.PreparedData, value.ConfigData
	if rollback {
		want, before = value.ConfigData, value.PreparedData
	}
	credential, err := k.request(ctx, http.MethodGet, k.path("secret", kubeName(value.Original.Endpoint)+"-credentials"), nil)
	if err != nil {
		return err
	}
	got, err := resetSecretData(credential)
	if err != nil || !owned(credential, value.Original.Project, value.Original.Endpoint) || nested(credential, "metadata", "uid") != value.Original.CredentialUID || !reflect.DeepEqual(got, value.CredentialData) {
		return errBranchResetBacking
	}
	path := k.path("secret", kubeName(value.Original.Endpoint)+"-config")
	for attempt := 0; attempt < 6; attempt++ {
		current, err := k.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		data, err := resetSecretData(current)
		if err != nil || !owned(current, value.Original.Project, value.Original.Endpoint) || nested(current, "metadata", "uid") != value.Original.ConfigUID {
			return errBranchResetBacking
		}
		if reflect.DeepEqual(data, want) {
			return nil
		}
		if !reflect.DeepEqual(data, before) {
			return errBranchResetBacking
		}
		current["data"] = resetDataAny(want)
		_, writeErr := k.request(ctx, http.MethodPut, path, current)
		observed, readErr := k.request(ctx, http.MethodGet, path, nil)
		if readErr != nil {
			if writeErr != nil {
				return writeErr
			}
			return readErr
		}
		data, readErr = resetSecretData(observed)
		if readErr != nil || !owned(observed, value.Original.Project, value.Original.Endpoint) || nested(observed, "metadata", "uid") != value.Original.ConfigUID {
			return errBranchResetBacking
		}
		if reflect.DeepEqual(data, want) {
			return nil
		}
		if !reflect.DeepEqual(data, before) {
			return errBranchResetBacking
		}
		var conflict kubeError
		if errors.As(writeErr, &conflict) && conflict.Status == http.StatusConflict {
			continue
		}
		if writeErr != nil {
			return writeErr
		}
		return errBranchResetBacking
	}
	return errBranchResetBacking
}
