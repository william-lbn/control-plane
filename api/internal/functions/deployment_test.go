package functions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestDeploymentCodeAndConfigOnlySnapshots(t *testing.T) {
	archive := archiveFor(t, "index.mjs")
	env := map[string]string{"KEEP": "original-private-value", "REMOVE": "delete-me", "CHANGE": "before"}
	first, err := ResolveDeployment(nil, DeploymentInput{Archive: archive, Environment: env})
	if err != nil {
		t.Fatal(err)
	}
	patch := map[string]string{"REMOVE": "", "CHANGE": "after-private-value", "ADD": "new-private-value"}
	second, err := ResolveDeployment(&first, DeploymentInput{Environment: patch})
	if err != nil {
		t.Fatal(err)
	}
	a, b := first.Summary(), second.Summary()
	if a.BundleDigest != b.BundleDigest || a.Entry != b.Entry || a.BundleBytes != b.BundleBytes || len(second.ArtifactBytes()) != 0 {
		t.Fatal("config-only deployment altered or duplicated code")
	}
	if got := second.SecretEnvironment(); got["KEEP"] != env["KEEP"] || got["CHANGE"] != patch["CHANGE"] || got["ADD"] != patch["ADD"] || len(got) != 3 {
		t.Fatal("deployment patch did not preserve, merge and delete variables")
	}
	if first.SecretEnvironment()["REMOVE"] != "delete-me" || first.SecretEnvironment()["CHANGE"] != "before" {
		t.Fatal("redeployment mutated prior immutable snapshot")
	}
	retained, err := RestoreDeploymentSnapshot(second.Summary(), second.SecretEnvironment())
	if err != nil || retained.Summary().BundleDigest != b.BundleDigest {
		t.Fatal("retained version could not hydrate after restart", err)
	}
	if _, err := RestoreDeploymentSnapshot(second.Summary(), map[string]string{"UNKNOWN": "retained-private-value"}); err == nil {
		t.Fatal("mismatched retained Secret accepted")
	}
	third, err := ResolveDeployment(&second, DeploymentInput{Archive: archiveFor(t, "index.js")})
	if err != nil || third.Summary().BundleDigest == second.Summary().BundleDigest || third.SecretEnvironment()["KEEP"] != env["KEEP"] {
		t.Fatal("code redeployment did not retain environment", err)
	}
}

func TestDeploymentCopiesCodeAndSecretsWithoutPublicDisclosure(t *testing.T) {
	archive := archiveFor(t, "index.mjs")
	original := append([]byte(nil), archive...)
	env := map[string]string{"PRIVATE_INPUT": "fixture-confidential-value"}
	s, err := ResolveDeployment(nil, DeploymentInput{Archive: archive, Environment: env})
	if err != nil {
		t.Fatal(err)
	}
	archive[0] ^= 1
	env["PRIVATE_INPUT"] = "changed"
	copy := s.SecretEnvironment()
	copy["PRIVATE_INPUT"] = "other"
	zipCopy := s.ArtifactBytes()
	zipCopy[0] ^= 1
	if !bytes.Equal(s.ArtifactBytes(), original) || s.SecretEnvironment()["PRIVATE_INPUT"] != "fixture-confidential-value" {
		t.Fatal("snapshot retains mutable caller aliases")
	}
	public, err := json.Marshal(s)
	if err != nil || bytes.Contains(public, []byte("fixture-confidential-value")) || bytes.Contains(public, []byte("export default")) || !bytes.Contains(public, []byte(`"environment_names":["PRIVATE_INPUT"]`)) {
		t.Fatal("public deployment serialization disclosed code or values")
	}
	input, _ := json.Marshal(DeploymentInput{Archive: original, Environment: s.SecretEnvironment()})
	if bytes.Contains(input, []byte("fixture-confidential-value")) || bytes.Contains(input, []byte("Archive")) {
		t.Fatal("input accidentally serializable into public Operations")
	}
}

func TestDeploymentRejectsFirstConfigOnlyInvalidArchiveAndReservedChanges(t *testing.T) {
	for _, input := range []DeploymentInput{{}, {Runtime: "python"}, {Archive: []byte("invalid archive")}, {Archive: archiveFor(t, "index.mjs"), Environment: map[string]string{"DATABASE_URL": "private-invalid-replacement"}}} {
		if _, err := ResolveDeployment(nil, input); err == nil || strings.Contains(err.Error(), "private-invalid-replacement") {
			t.Fatal("invalid deployment accepted or secret disclosed")
		}
	}
	first, err := ResolveDeployment(nil, DeploymentInput{Archive: archiveFor(t, "index.mjs")})
	if err != nil {
		t.Fatal(err)
	}
	first.digest = strings.Repeat("z", 64)
	if _, err := ResolveDeployment(&first, DeploymentInput{}); err == nil {
		t.Fatal("corrupt previous deployment accepted")
	}
}

func TestDeploymentMergedEnvironmentBudget(t *testing.T) {
	env := make(map[string]string)
	for i := 0; i < 32; i++ {
		env[fmt.Sprintf("K%d", i)] = "value"
	}
	first, err := ResolveDeployment(nil, DeploymentInput{Archive: archiveFor(t, "index.mjs"), Environment: env})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveDeployment(&first, DeploymentInput{Environment: map[string]string{"EXTRA": "value"}}); err == nil {
		t.Fatal("separate small patches exceeded aggregate environment count")
	}
	second, err := ResolveDeployment(&first, DeploymentInput{Environment: map[string]string{"K0": "", "EXTRA": "value"}})
	if err != nil || len(second.SecretEnvironment()) != 32 {
		t.Fatal("delete-and-add within aggregate budget failed", err)
	}
}

func TestDeploymentFingerprintBindsSecretsCodeScopeAndVersion(t *testing.T) {
	key := bytes.Repeat([]byte{19}, 32)
	scope := DeploymentRequestScope{ProjectID: "prj_0000000000000001", BranchID: "br_0000000000000002", Slug: "hello", ExpectedVersion: 3}
	input := DeploymentInput{Archive: archiveFor(t, "index.mjs"), Environment: map[string]string{"PRIVATE_INPUT": "private-original"}}
	first, err := input.RequestFingerprint(key, scope)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := input.RequestFingerprint(key, scope)
	if err != nil || first != repeated {
		t.Fatal("identical request fingerprint changed", err)
	}
	input.Environment["PRIVATE_INPUT"] = "private-replacement"
	second, err := input.RequestFingerprint(key, scope)
	if err != nil || first == second {
		t.Fatal("secret rotation reused the old request fingerprint", err)
	}
	for _, changed := range []DeploymentRequestScope{
		{ProjectID: scope.ProjectID, BranchID: "br_0000000000000003", Slug: scope.Slug, ExpectedVersion: scope.ExpectedVersion},
		{ProjectID: scope.ProjectID, BranchID: scope.BranchID, Slug: scope.Slug, ExpectedVersion: scope.ExpectedVersion + 1},
		{ProjectID: scope.ProjectID, BranchID: scope.BranchID, Slug: "other", ExpectedVersion: scope.ExpectedVersion},
	} {
		got, err := input.RequestFingerprint(key, changed)
		if err != nil || got == second {
			t.Fatal("request fingerprint did not bind branch, version or slug", err)
		}
	}
	input.Archive = archiveFor(t, "index.js")
	third, err := input.RequestFingerprint(key, scope)
	if err != nil || third == second {
		t.Fatal("code update reused old request fingerprint", err)
	}
	if _, err := input.RequestFingerprint(nil, scope); err == nil {
		t.Fatal("missing fingerprint key accepted")
	}
}
