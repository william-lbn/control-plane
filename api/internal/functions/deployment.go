package functions

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
)

type DeploymentRequestScope struct {
	ProjectID       string `json:"project_id"`
	BranchID        string `json:"branch_id"`
	Slug            string `json:"slug"`
	ExpectedVersion int64  `json:"expected_version"`
}

// DeploymentInput is the bounded result of an authenticated multipart parser.
// Omitted archive reuses a known previous bundle; omitted env preserves values,
// while an empty patch value deletes the named user variable. Code/values are
// never serialized into an Operation, public response, or audit event.
type DeploymentInput struct {
	Runtime     string            `json:"runtime"`
	Archive     []byte            `json:"-"`
	Environment map[string]string `json:"-"`
}

// RequestFingerprint binds secret values as well as code and If-Match identity.
// MarshalJSON of an input/snapshot intentionally omits secrets and must NOT be
// used to fingerprint a request. Only this domain-separated MAC is retained.
func (input DeploymentInput) RequestFingerprint(key []byte, scope DeploymentRequestScope) (string, error) {
	if len(key) < 32 || !projectPattern.MatchString(scope.ProjectID) || !branchPattern.MatchString(scope.BranchID) || !ValidSlug(scope.Slug) || scope.ExpectedVersion < 0 || (input.Runtime != "" && input.Runtime != Runtime) || len(input.Archive) > MaxArchiveBytes {
		return "", errors.New("invalid Functions deployment request fingerprint scope")
	}
	if input.Environment != nil && ValidateEnvironment(input.Environment) != nil {
		return "", errors.New("invalid Functions deployment environment patch")
	}
	digest := ""
	if len(input.Archive) > 0 {
		value := sha256.Sum256(input.Archive)
		digest = hex.EncodeToString(value[:])
	}
	body, err := json.Marshal(struct {
		Scope               DeploymentRequestScope `json:"scope"`
		Runtime             string                 `json:"runtime"`
		ArchiveDigest       string                 `json:"archive_digest"`
		EnvironmentSupplied bool                   `json:"environment_supplied"`
		Environment         map[string]string      `json:"environment"`
	}{scope, input.Runtime, digest, input.Environment != nil, input.Environment})
	if err != nil {
		return "", errors.New("Functions deployment fingerprint unavailable")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("neon.functions.deployment-request.v1\x00"))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// DeploymentSnapshot is immutable after admission. Private bytes must be stored
// in the artifact store and immutable Secret before an intent references them.
// Its JSON representation deliberately contains metadata and env names only.
type DeploymentSnapshot struct {
	runtime     string
	digest      string
	entry       string
	size        int
	archive     []byte
	environment map[string]string
}

type DeploymentSummary struct {
	Runtime          string   `json:"runtime"`
	BundleDigest     string   `json:"bundle_digest"`
	Entry            string   `json:"entry"`
	BundleBytes      int      `json:"bundle_bytes"`
	EnvironmentNames []string `json:"environment_names"`
}

func (s DeploymentSnapshot) Summary() DeploymentSummary {
	names := make([]string, 0, len(s.environment))
	for name := range s.environment {
		names = append(names, name)
	}
	sort.Strings(names)
	return DeploymentSummary{Runtime: s.runtime, BundleDigest: s.digest, Entry: s.entry, BundleBytes: s.size, EnvironmentNames: names}
}
func (s DeploymentSnapshot) MarshalJSON() ([]byte, error) { return json.Marshal(s.Summary()) }

// SecretEnvironment returns a detached copy only for immutable Secret creation.
// The Driver must not grant readers access to this Secret or log this map.
func (s DeploymentSnapshot) SecretEnvironment() map[string]string {
	result := make(map[string]string, len(s.environment))
	for name, value := range s.environment {
		result[name] = value
	}
	return result
}

// ArtifactBytes is nonempty only for a code deployment. A config-only update
// keeps the exact prior content digest without downloading/executing it again.
func (s DeploymentSnapshot) ArtifactBytes() []byte { return append([]byte(nil), s.archive...) }

// RestoreDeploymentSnapshot hydrates a retained immutable metadata summary and
// an ownership-verified Secret after process restart. It validates the summary
// against the Secret names; it does not authenticate either backing resource.
func RestoreDeploymentSnapshot(summary DeploymentSummary, environment map[string]string) (DeploymentSnapshot, error) {
	if summary.Runtime != Runtime || !digestPattern.MatchString(summary.BundleDigest) || (summary.Entry != "index.mjs" && summary.Entry != "index.js") || summary.BundleBytes < 1 || summary.BundleBytes > MaxArchiveBytes || ValidateEnvironment(environment) != nil {
		return DeploymentSnapshot{}, errors.New("invalid retained Functions deployment")
	}
	result := DeploymentSnapshot{runtime: summary.Runtime, digest: summary.BundleDigest, entry: summary.Entry, size: summary.BundleBytes, environment: make(map[string]string, len(environment))}
	for name, value := range environment {
		result.environment[name] = value
	}
	names := result.Summary().EnvironmentNames
	if len(names) != len(summary.EnvironmentNames) {
		return DeploymentSnapshot{}, errors.New("Functions deployment Secret metadata mismatch")
	}
	for i, name := range names {
		if name != summary.EnvironmentNames[i] {
			return DeploymentSnapshot{}, errors.New("Functions deployment Secret metadata mismatch")
		}
	}
	return result, nil
}

func ResolveDeployment(previous *DeploymentSnapshot, input DeploymentInput) (DeploymentSnapshot, error) {
	var result DeploymentSnapshot
	if input.Runtime != "" && input.Runtime != Runtime {
		return result, errors.New("only nodejs24 Functions runtime is supported")
	}
	// Multipart omission preserves values. The final immutable Secret always
	// contains an explicit non-null map, as the guest bootstrap requires.
	if input.Environment == nil {
		input.Environment = map[string]string{}
	}
	if ValidateEnvironment(input.Environment) != nil {
		return result, errors.New("invalid bounded user environment patch")
	}
	result.runtime = Runtime
	result.environment = make(map[string]string)
	if previous != nil {
		if previous.runtime != Runtime || !digestPattern.MatchString(previous.digest) || (previous.entry != "index.mjs" && previous.entry != "index.js") || previous.size < 1 || previous.size > MaxArchiveBytes || ValidateEnvironment(previous.environment) != nil {
			return DeploymentSnapshot{}, errors.New("previous immutable Functions deployment is invalid")
		}
		result.digest = previous.digest
		result.entry = previous.entry
		result.size = previous.size
		for name, value := range previous.environment {
			result.environment[name] = value
		}
	}
	for name, value := range input.Environment {
		if value == "" {
			delete(result.environment, name)
		} else {
			result.environment[name] = value
		}
	}
	if ValidateEnvironment(result.environment) != nil {
		return DeploymentSnapshot{}, errors.New("merged Functions environment exceeds bounds")
	}
	if len(input.Archive) > 0 {
		bundle, err := ValidateBundle(input.Archive)
		if err != nil {
			return DeploymentSnapshot{}, err
		}
		result.digest = bundle.Digest
		result.entry = bundle.Entry
		result.size = len(input.Archive)
		result.archive = append([]byte(nil), input.Archive...)
	} else if previous == nil {
		return DeploymentSnapshot{}, errors.New("first Functions deployment requires a validated ZIP bundle")
	}
	return result, nil
}
