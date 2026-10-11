package control

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

// This is internal immutable backing, not a deployment admission endpoint.
// The caller must authorize the branch and verify the Operation lease before
// attaching this reference to an intent. No environment values enter metadata,
// an Operation, an audit entry, a public response or error text.
type functionEnvironmentReference struct {
	Namespace    string `json:"-"`
	ProjectID    string `json:"-"`
	BranchID     string `json:"-"`
	DeploymentID string `json:"-"`
	Name         string `json:"-"`
	UID          string `json:"-"`
	MAC          string `json:"-"`
}

var errFunctionEnvironment = errors.New("Functions immutable environment unavailable or ownership rejected")
var functionEnvironmentNamespace = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

func (r functionEnvironmentReference) valid() bool {
	return functionEnvironmentNamespace.MatchString(r.Namespace) && functionArtifactProject.MatchString(r.ProjectID) && functionArtifactBranch.MatchString(r.BranchID) &&
		functionDeploymentID.MatchString(r.DeploymentID) && r.Name == "fdp-"+r.DeploymentID[4:]+"-environment" &&
		(r.UID == "" || functionResourceUID.MatchString(r.UID))
}

func functionEnvironmentMAC(key []byte, ref functionEnvironmentReference, environment map[string]string) (string, []byte, error) {
	if len(key) < 32 || !ref.valid() || environment == nil || functions.ValidateEnvironment(environment) != nil {
		return "", nil, errFunctionEnvironment
	}
	// encoding/json sorts map keys, so a process restart reproduces the same
	// bytes. Bound both canonical bytes and their scope; never use a plain hash
	// of low-entropy user secrets as a public fingerprint.
	data, err := json.Marshal(environment)
	if err != nil || len(data) > functions.MaxBootstrapBytes {
		return "", nil, errFunctionEnvironment
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("neon.functions.environment.v1\x00" + ref.Namespace + "\x00" + ref.ProjectID + "\x00" + ref.BranchID + "\x00" + ref.DeploymentID + "\x00"))
	_, _ = mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil)), data, nil
}

func functionEnvironmentLabels(ref functionEnvironmentReference) map[string]string {
	return map[string]string{
		"app.kubernetes.io/part-of": "neon-control", "neon-control/backing-kind": "function-environment-v1",
		"neon-control/project-id": ref.ProjectID, "neon-control/branch-id": ref.BranchID,
		"neon-control/deployment-id": ref.DeploymentID,
	}
}

func verifyFunctionEnvironment(item map[string]any, ref functionEnvironmentReference, key []byte) (functionEnvironmentReference, map[string]string, error) {
	if !ref.valid() || len(key) < 32 || !functionArtifactDigest.MatchString(ref.MAC) ||
		stringVal(nested(item, "metadata", "namespace")) != ref.Namespace ||
		stringVal(nested(item, "metadata", "name")) != ref.Name || item["immutable"] != true || item["type"] != "Opaque" ||
		nested(item, "metadata", "deletionTimestamp") != nil {
		return functionEnvironmentReference{}, nil, errFunctionEnvironment
	}
	uid := stringVal(nested(item, "metadata", "uid"))
	if !functionResourceUID.MatchString(uid) || (ref.UID != "" && ref.UID != uid) {
		return functionEnvironmentReference{}, nil, errFunctionEnvironment
	}
	for name, value := range functionEnvironmentLabels(ref) {
		if stringVal(nested(item, "metadata", "labels", name)) != value {
			return functionEnvironmentReference{}, nil, errFunctionEnvironment
		}
	}
	data, ok := item["data"].(map[string]any)
	if !ok || len(data) != 1 {
		return functionEnvironmentReference{}, nil, errFunctionEnvironment
	}
	raw, err := base64.StdEncoding.DecodeString(stringVal(data["environment.json"]))
	if err != nil || len(raw) < 2 || len(raw) > functions.MaxBootstrapBytes {
		return functionEnvironmentReference{}, nil, errFunctionEnvironment
	}
	var environment map[string]string
	if json.Unmarshal(raw, &environment) != nil || environment == nil {
		return functionEnvironmentReference{}, nil, errFunctionEnvironment
	}
	mac, canonical, err := functionEnvironmentMAC(key, ref, environment)
	if err != nil || !bytes.Equal(raw, canonical) || !hmac.Equal([]byte(mac), []byte(ref.MAC)) ||
		!hmac.Equal([]byte(mac), []byte(stringVal(nested(item, "metadata", "annotations", "neon-control/environment-mac")))) {
		return functionEnvironmentReference{}, nil, errFunctionEnvironment
	}
	ref.UID = uid
	return ref, environment, nil
}

// GET before creation, POST at most once, then GET on every unknown/conflict
// result. Kubernetes may have committed a POST whose response was lost. Never
// retry POST blindly, patch data, replace a UID or delete mismatched backing.
func (k *kubeClient) reserveFunctionEnvironment(ctx context.Context, key []byte, project, branch, deployment string, environment map[string]string) (functionEnvironmentReference, error) {
	if k == nil {
		return functionEnvironmentReference{}, errFunctionEnvironment
	}
	ref := functionEnvironmentReference{Namespace: k.namespace, ProjectID: project, BranchID: branch, DeploymentID: deployment}
	if functionDeploymentID.MatchString(deployment) {
		ref.Name = "fdp-" + deployment[4:] + "-environment"
	}
	mac, data, err := functionEnvironmentMAC(key, ref, environment)
	if err != nil || k == nil {
		return functionEnvironmentReference{}, errFunctionEnvironment
	}
	ref.MAC = mac
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	item, err := k.request(ctx, http.MethodGet, k.path("secret", ref.Name), nil)
	if err != nil {
		var ke kubeError
		if !errors.As(err, &ke) || ke.Status != http.StatusNotFound {
			return functionEnvironmentReference{}, errFunctionEnvironment
		}
		body := map[string]any{
			"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "immutable": true,
			"metadata": map[string]any{"name": ref.Name, "namespace": k.namespace, "labels": functionEnvironmentLabels(ref),
				"annotations": map[string]string{"neon-control/environment-mac": mac}},
			"data": map[string]string{"environment.json": base64.StdEncoding.EncodeToString(data)},
		}
		item, err = k.request(ctx, http.MethodPost, k.path("secret", ""), body)
		if err != nil {
			item, err = k.request(ctx, http.MethodGet, k.path("secret", ref.Name), nil)
		}
		if err != nil {
			return functionEnvironmentReference{}, errFunctionEnvironment
		}
	}
	verified, _, err := verifyFunctionEnvironment(item, ref, key)
	return verified, err
}

func (k *kubeClient) readFunctionEnvironment(ctx context.Context, key []byte, ref functionEnvironmentReference) (map[string]string, error) {
	if k == nil || !ref.valid() || ref.UID == "" {
		return nil, errFunctionEnvironment
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	item, err := k.request(ctx, http.MethodGet, k.path("secret", ref.Name), nil)
	if err != nil {
		return nil, errFunctionEnvironment
	}
	_, environment, err := verifyFunctionEnvironment(item, ref, key)
	return environment, err
}
