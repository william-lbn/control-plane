//go:build live

package control

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"
	"time"
)

// Actual Kubernetes identity/admission/unknown-reply acceptance only. The
// generated scope is an isolated test fixture; no business metadata or SQL-role
// write is made. The immutable Secret stays retained as evidence.
func TestFunctionSQLSecretLiveKubernetes(t *testing.T) {
	if os.Getenv("NEON_FUNCTIONS_LIVE_SQL_SECRET_TEST") != "true" {
		t.Fatal("explicit live SQL credential admission required")
	}
	functionID := os.Getenv("NEON_FUNCTIONS_LIVE_FUNCTION_ID")
	if !functionDefinitionID.MatchString(functionID) {
		t.Fatal("explicit owned test function required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	k, err := newKubeClient()
	if err != nil {
		t.Fatal("dedicated projected identity unavailable")
	}
	ref, c, _ := functionSQLSecretFixture(t)
	ref.Namespace = k.namespace
	ref.Scope.FunctionID = functionID
	ref.Name = "fn-" + strings.TrimPrefix(functionID, "fnc_") + "-sql-1"
	ref.Scope.ProjectID = "prj_" + randomToken(8)
	ref.Scope.BranchID = "br_" + randomToken(8)
	ref.WriterID = "ep_" + randomToken(8)
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		t.Fatal("private test MAC key unavailable")
	}
	c.Password = randomToken(32)
	c.Verifier, err = scramVerifier(c.Password)
	if err != nil {
		t.Fatal("private test verifier unavailable")
	}
	loss := &functionLiveLostSecretReply{delegate: k.http.Transport}
	k.http.Transport = loss
	original, err := k.reserveFunctionSQLSecret(ctx, key, ref, c)
	if err != nil || original.Reference.UID == "" || !loss.lost {
		t.Fatal("actual accepted SQL Secret did not recover unknown reply")
	}
	restarted, err := newKubeClient()
	if err != nil {
		t.Fatal("fresh projected identity unavailable")
	}
	recovered, err := restarted.findFunctionSQLSecret(ctx, key, ref)
	if err != nil || recovered != original {
		t.Fatal("actual original SQL identity changed after restart")
	}
	if _, err = restarted.readFunctionSQLSecret(ctx, key, original.Reference); err != nil {
		t.Fatal("actual recorded SQL identity unreadable")
	}
	wrong := original.Reference
	wrong.UID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if _, err = restarted.readFunctionSQLSecret(ctx, key, wrong); err == nil {
		t.Fatal("replacement SQL Secret UID adopted")
	}
	wrong = ref
	wrong.Scope.BranchID = "br_" + randomToken(8)
	if _, err = restarted.findFunctionSQLSecret(ctx, key, wrong); err == nil {
		t.Fatal("foreign branch SQL identity adopted")
	}
	changed := c
	changed.Password = randomToken(32)
	if _, err = restarted.reserveFunctionSQLSecret(ctx, key, ref, changed); err == nil {
		t.Fatal("original SQL password overwritten or adopted for different proposal")
	}
	t.Logf("retained SQL credential namespace=%s name=%s uid=%s function=%s", ref.Namespace, ref.Name, original.Reference.UID, functionID)
}
