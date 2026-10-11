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
	"strconv"
	"strings"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

// A credential belongs to a Function definition, not an execution instance.
// Reusing it across deployments must not rotate a still-running guest's login.
// The future Driver must persist the verified original UID/MAC before SQL or
// bootstrap publication. These primitives alone do not authorize deployment.
type functionSQLReference struct {
	Namespace, Name, UID, MAC, MACKeyVersion string            `json:"-"`
	WriterID                                 string            `json:"-"`
	Generation                               int64             `json:"-"`
	Scope                                    functionsql.Scope `json:"-"`
}

type functionSQLCredential struct {
	Password string `json:"password"`
	Verifier string `json:"verifier"`
}

// Never marshal this record. Only credentialJSON may be stored in the private
// immutable Secret; no password/verifier belongs in metadata or API responses.
type functionSQLRecord struct {
	Reference  functionSQLReference  `json:"-"`
	Credential functionSQLCredential `json:"-"`
}

var errFunctionSQLSecret = errors.New("Functions immutable SQL credential unavailable or ownership rejected")
var functionSQLPassword = regexp.MustCompile(`^[A-Za-z0-9_-]{32,128}$`)

func (r functionSQLReference) valid() bool {
	return r.Scope.Validate() == nil && r.Scope.ProvisioningRole != "" && functionEnvironmentNamespace.MatchString(r.Namespace) &&
		r.Generation > 0 && r.Name == "fn-"+strings.TrimPrefix(r.Scope.FunctionID, "fnc_")+"-sql-"+strconv.FormatInt(r.Generation, 10) &&
		functionWriterEndpointID.MatchString(r.WriterID) && functionMACKeyVersion.MatchString(r.MACKeyVersion) &&
		(r.MAC == "" || functionArtifactDigest.MatchString(r.MAC)) &&
		(r.UID == "" || (functionResourceUID.MatchString(r.UID) && functionArtifactDigest.MatchString(r.MAC)))
}

func functionSQLLabels(r functionSQLReference) map[string]string {
	return map[string]string{"app.kubernetes.io/part-of": "neon-control", "neon-control/backing-kind": "function-sql-v1",
		"neon-control/project-id": r.Scope.ProjectID, "neon-control/branch-id": r.Scope.BranchID,
		"neon-control/function-id": r.Scope.FunctionID}
}

func functionSQLContent(key []byte, r functionSQLReference, c functionSQLCredential) (string, []byte, error) {
	if len(key) < 32 || !r.valid() || !functionSQLPassword.MatchString(c.Password) || !functionsql.ValidVerifier(c.Verifier) {
		return "", nil, errFunctionSQLSecret
	}
	data, err := json.Marshal(c)
	if err != nil {
		return "", nil, errFunctionSQLSecret
	}
	mac := hmac.New(sha256.New, key)
	// Every scope component is authenticated, including the trusted creator,
	// original Writer and key version. Name/namespace bind immutable storage.
	for _, v := range []string{"neon.functions.sql.v1", r.Namespace, r.Name, r.Scope.FunctionID, r.Scope.ProjectID, r.Scope.BranchID,
		r.Scope.Database, r.Scope.Schema, r.Scope.ProvisioningRole, r.WriterID, strconv.FormatInt(r.Generation, 10), r.MACKeyVersion} {
		_, _ = mac.Write([]byte(v))
		_, _ = mac.Write([]byte{0})
	}
	_, _ = mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil)), data, nil
}

func verifyFunctionSQLSecret(item map[string]any, key []byte, ref functionSQLReference) (functionSQLRecord, error) {
	var empty functionSQLRecord
	if !ref.valid() || len(key) < 32 || stringVal(nested(item, "metadata", "namespace")) != ref.Namespace ||
		stringVal(nested(item, "metadata", "name")) != ref.Name || item["immutable"] != true || item["type"] != "Opaque" ||
		nested(item, "metadata", "deletionTimestamp") != nil {
		return empty, errFunctionSQLSecret
	}
	uid := stringVal(nested(item, "metadata", "uid"))
	if !functionResourceUID.MatchString(uid) || (ref.UID != "" && ref.UID != uid) {
		return empty, errFunctionSQLSecret
	}
	for k, v := range functionSQLLabels(ref) {
		if stringVal(nested(item, "metadata", "labels", k)) != v {
			return empty, errFunctionSQLSecret
		}
	}
	if stringVal(nested(item, "metadata", "annotations", "neon-control/sql-key-version")) != ref.MACKeyVersion {
		return empty, errFunctionSQLSecret
	}
	values, ok := item["data"].(map[string]any)
	if !ok || len(values) != 1 {
		return empty, errFunctionSQLSecret
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(stringVal(values["sql.json"]))
	if err != nil || len(raw) < 2 || len(raw) > 4096 {
		return empty, errFunctionSQLSecret
	}
	var c functionSQLCredential
	if json.Unmarshal(raw, &c) != nil {
		return empty, errFunctionSQLSecret
	}
	mac, canonical, err := functionSQLContent(key, ref, c)
	if err != nil || !bytes.Equal(raw, canonical) || !hmac.Equal([]byte(mac), []byte(stringVal(nested(item, "metadata", "annotations", "neon-control/sql-content-mac")))) ||
		(ref.MAC != "" && !hmac.Equal([]byte(ref.MAC), []byte(mac))) {
		return empty, errFunctionSQLSecret
	}
	ref.UID = uid
	ref.MAC = mac
	return functionSQLRecord{Reference: ref, Credential: c}, nil
}

// A stable proposed credential is created at most once. An unknown POST result
// is resolved by GET, not a second POST. A different same-name credential is
// rejected, never patched/replaced; explicit find handles the original result.
func (k *kubeClient) reserveFunctionSQLSecret(ctx context.Context, key []byte, ref functionSQLReference, c functionSQLCredential) (functionSQLRecord, error) {
	var empty functionSQLRecord
	mac, data, err := functionSQLContent(key, ref, c)
	if k == nil || err != nil || ref.Namespace != k.namespace || ref.UID != "" || ref.MAC != "" {
		return empty, errFunctionSQLSecret
	}
	ref.MAC = mac
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	item, err := k.request(ctx, http.MethodGet, k.path("secret", ref.Name), nil)
	if err != nil {
		var ke kubeError
		if !errors.As(err, &ke) || ke.Status != http.StatusNotFound {
			return empty, errFunctionSQLSecret
		}
		body := map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "immutable": true,
			"metadata": map[string]any{"name": ref.Name, "namespace": ref.Namespace, "labels": functionSQLLabels(ref),
				"annotations": map[string]string{"neon-control/sql-content-mac": mac, "neon-control/sql-key-version": ref.MACKeyVersion}},
			"data": map[string]string{"sql.json": base64.StdEncoding.EncodeToString(data)}}
		item, err = k.request(ctx, http.MethodPost, k.path("secret", ""), body)
		if err != nil {
			item, err = k.request(ctx, http.MethodGet, k.path("secret", ref.Name), nil)
		}
		if err != nil {
			return empty, errFunctionSQLSecret
		}
	}
	return verifyFunctionSQLSecret(item, key, ref)
}

// Explicit recovery after an original proposal was accepted but its reply or
// local credential was lost. No write occurs. The caller must use the original
// trusted scope/key and persist the recovered real UID, never guessed metadata.
func (k *kubeClient) findFunctionSQLSecret(ctx context.Context, key []byte, ref functionSQLReference) (functionSQLRecord, error) {
	if k == nil || !ref.valid() || ref.Namespace != k.namespace {
		return functionSQLRecord{}, errFunctionSQLSecret
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	item, err := k.request(ctx, http.MethodGet, k.path("secret", ref.Name), nil)
	if err != nil {
		return functionSQLRecord{}, errFunctionSQLSecret
	}
	return verifyFunctionSQLSecret(item, key, ref)
}

func (k *kubeClient) readFunctionSQLSecret(ctx context.Context, key []byte, ref functionSQLReference) (functionSQLRecord, error) {
	if ref.UID == "" || ref.MAC == "" {
		return functionSQLRecord{}, errFunctionSQLSecret
	}
	return k.findFunctionSQLSecret(ctx, key, ref)
}
