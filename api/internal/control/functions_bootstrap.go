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
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

// The trusted Driver obtains this contract from leased immutable metadata.
// Neither the guest nor a URL can supply the Function/Writer association.
// It contains no credential values and is still private to the Driver.
type functionBootstrapReference struct {
	Namespace     string          `json:"-"`
	Name          string          `json:"-"`
	UID           string          `json:"-"`
	MAC           string          `json:"-"`
	MACKeyVersion string          `json:"-"`
	FunctionID    string          `json:"-"`
	WriterID      string          `json:"-"`
	BundleDigest  string          `json:"-"`
	Scope         functions.Scope `json:"-"`
}

// Do not serialize the Bootstrap: it contains manager/artifact/SQL private
// material. Only the root-only Secret CD-ROM is allowed to receive its bytes.
type functionBootstrapRecord struct {
	Reference functionBootstrapReference `json:"-"`
	Config    functions.Bootstrap        `json:"-"`
}

var errFunctionBootstrap = errors.New("Functions immutable bootstrap unavailable or ownership rejected")
var functionWriterEndpointID = regexp.MustCompile(`^ep_[a-f0-9]{16}$`)

func (r functionBootstrapReference) valid() bool {
	return r.Scope.Validate() == nil && functionEnvironmentNamespace.MatchString(r.Namespace) &&
		r.Name == "fn-"+strings.TrimPrefix(r.Scope.InstanceID, "fni_")+"-bootstrap" &&
		functionDefinitionID.MatchString(r.FunctionID) && functionArtifactDigest.MatchString(r.BundleDigest) &&
		functionWriterEndpointID.MatchString(r.WriterID) &&
		functionMACKeyVersion.MatchString(r.MACKeyVersion) && (r.MAC == "" || functionArtifactDigest.MatchString(r.MAC)) &&
		(r.UID == "" || (functionResourceUID.MatchString(r.UID) && functionArtifactDigest.MatchString(r.MAC)))
}

func functionBootstrapLabels(r functionBootstrapReference) map[string]string {
	return map[string]string{"app.kubernetes.io/part-of": "neon-control", "neon-control/backing-kind": "function-bootstrap-v1",
		"neon-control/project-id": r.Scope.ProjectID, "neon-control/branch-id": r.Scope.BranchID, "neon-control/function-id": r.FunctionID,
		"neon-control/deployment-id": r.Scope.DeploymentID, "neon-control/instance-id": r.Scope.InstanceID}
}

func functionBootstrapContent(key []byte, r functionBootstrapReference, b functions.Bootstrap) (string, []byte, error) {
	if len(key) < 32 || !r.valid() || b.Validate() != nil || b.Scope != r.Scope || b.BundleDigest != r.BundleDigest || b.WriterEndpointID != r.WriterID {
		return "", nil, errFunctionBootstrap
	}
	u, err := url.Parse(b.DatabaseURL)
	if err != nil || u.User == nil || u.User.Username() != "fn_"+strings.TrimPrefix(r.FunctionID, "fnc_") {
		return "", nil, errFunctionBootstrap
	}
	data, err := json.Marshal(b)
	if err != nil || len(data) > functions.MaxBootstrapBytes {
		return "", nil, errFunctionBootstrap
	}
	// Bind key version as well as every scoped byte. A restart must retain the
	// original keys/certificate, rather than generating a new identity in place.
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("neon.functions.bootstrap.v1\x00" + r.Namespace + "\x00" + r.Name + "\x00" + r.FunctionID + "\x00" + r.MACKeyVersion + "\x00"))
	_, _ = mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil)), data, nil
}

func verifyFunctionBootstrap(item map[string]any, key []byte, ref functionBootstrapReference) (functionBootstrapRecord, error) {
	var empty functionBootstrapRecord
	if !ref.valid() || len(key) < 32 || stringVal(nested(item, "metadata", "namespace")) != ref.Namespace ||
		stringVal(nested(item, "metadata", "name")) != ref.Name || item["immutable"] != true || item["type"] != "Opaque" || nested(item, "metadata", "deletionTimestamp") != nil {
		return empty, errFunctionBootstrap
	}
	uid := stringVal(nested(item, "metadata", "uid"))
	if !functionResourceUID.MatchString(uid) || (ref.UID != "" && ref.UID != uid) {
		return empty, errFunctionBootstrap
	}
	for name, value := range functionBootstrapLabels(ref) {
		if stringVal(nested(item, "metadata", "labels", name)) != value {
			return empty, errFunctionBootstrap
		}
	}
	if stringVal(nested(item, "metadata", "annotations", "neon-control/bootstrap-key-version")) != ref.MACKeyVersion {
		return empty, errFunctionBootstrap
	}
	data, ok := item["data"].(map[string]any)
	if !ok || len(data) != 1 {
		return empty, errFunctionBootstrap
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(stringVal(data["config.json"]))
	if err != nil || len(raw) == 0 || len(raw) > functions.MaxBootstrapBytes {
		return empty, errFunctionBootstrap
	}
	config, err := functions.DecodeBootstrap(raw)
	if err != nil {
		return empty, errFunctionBootstrap
	}
	mac, canonical, err := functionBootstrapContent(key, ref, config)
	if err != nil || !bytes.Equal(raw, canonical) || !hmac.Equal([]byte(mac), []byte(stringVal(nested(item, "metadata", "annotations", "neon-control/bootstrap-mac")))) ||
		(ref.MAC != "" && !hmac.Equal([]byte(mac), []byte(ref.MAC))) {
		return empty, errFunctionBootstrap
	}
	ref.UID, ref.MAC = uid, mac
	return functionBootstrapRecord{Reference: ref, Config: config}, nil
}

// Recovery before a receipt commit is deliberately explicit: find the original
// immutable, scope-bound and MAC-verified Secret before issuing any new leaf or
// keys. A non-404 read is never permission to create. Callers must hold a live
// metadata lease and validate the retained trust/config policy before use.
func (k *kubeClient) findFunctionBootstrap(ctx context.Context, key []byte, ref functionBootstrapReference) (functionBootstrapRecord, bool, error) {
	var empty functionBootstrapRecord
	if k == nil || !ref.valid() || ref.Namespace != k.namespace || len(key) < 32 {
		return empty, false, errFunctionBootstrap
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	item, err := k.request(ctx, http.MethodGet, k.path("secret", ref.Name), nil)
	if err != nil {
		var ke kubeError
		if errors.As(err, &ke) && ke.Status == http.StatusNotFound && ref.UID == "" {
			return empty, false, nil
		}
		return empty, false, errFunctionBootstrap
	}
	record, err := verifyFunctionBootstrap(item, key, ref)
	if err != nil {
		return empty, false, errFunctionBootstrap
	}
	return record, true, nil
}

// A new create is admitted only with canonical validated content. POST is sent
// at most once; lost acknowledgement/conflict is resolved by GET. Existing
// content must match exactly, and is never patched/deleted or re-keyed here.
func (k *kubeClient) reserveFunctionBootstrap(ctx context.Context, key []byte, ref functionBootstrapReference, config functions.Bootstrap) (functionBootstrapRecord, error) {
	var empty functionBootstrapRecord
	if k == nil || ref.Namespace != k.namespace {
		return empty, errFunctionBootstrap
	}
	mac, data, err := functionBootstrapContent(key, ref, config)
	if err != nil || (ref.MAC != "" && !hmac.Equal([]byte(mac), []byte(ref.MAC))) {
		return empty, errFunctionBootstrap
	}
	ref.MAC = mac
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	existing, found, err := k.findFunctionBootstrap(ctx, key, ref)
	if err != nil || found {
		return existing, err
	}
	// A persisted UID is proof that this instance was already created. Its loss
	// must stop recovery, never authorize a replacement of the same Secret name.
	if ref.UID != "" {
		return empty, errFunctionBootstrap
	}
	body := map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "immutable": true,
		"metadata": map[string]any{"namespace": ref.Namespace, "name": ref.Name, "labels": functionBootstrapLabels(ref),
			"annotations": map[string]string{"neon-control/bootstrap-mac": mac, "neon-control/bootstrap-key-version": ref.MACKeyVersion}},
		"data": map[string]string{"config.json": base64.StdEncoding.EncodeToString(data)}}
	item, err := k.request(ctx, http.MethodPost, k.path("secret", ""), body)
	if err != nil {
		item, err = k.request(ctx, http.MethodGet, k.path("secret", ref.Name), nil)
	}
	if err != nil {
		return empty, errFunctionBootstrap
	}
	return verifyFunctionBootstrap(item, key, ref)
}

func (k *kubeClient) readFunctionBootstrap(ctx context.Context, key []byte, ref functionBootstrapReference) (functionBootstrapRecord, error) {
	if ref.UID == "" || !functionArtifactDigest.MatchString(ref.MAC) {
		return functionBootstrapRecord{}, errFunctionBootstrap
	}
	item, found, err := k.findFunctionBootstrap(ctx, key, ref)
	if err != nil || !found {
		return functionBootstrapRecord{}, errFunctionBootstrap
	}
	return item, nil
}
