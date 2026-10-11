//go:build live

package control

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

// This is a real API-server backing test, not a Function deployment. The
// projected identity is restricted to create plus GET this exact test name.
// The accepted Secret, original UID and private keys remain retained evidence.
func TestFunctionBootstrapLiveKubernetes(t *testing.T) {
	if os.Getenv("NEON_FUNCTIONS_LIVE_BOOTSTRAP_TEST") != "true" {
		t.Fatal("explicit live bootstrap admission required")
	}
	scope := functions.Scope{InstanceID: os.Getenv("NEON_FUNCTIONS_LIVE_INSTANCE_ID"), DeploymentID: os.Getenv("NEON_FUNCTIONS_LIVE_DEPLOYMENT_ID"), ProjectID: "prj_" + randomToken(8), BranchID: "br_" + randomToken(8), Slug: "hello", Generation: 1}
	if scope.Validate() != nil {
		t.Fatal("explicit owned test instance/deployment required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	k, err := newKubeClient()
	if err != nil {
		t.Fatal("dedicated projected identity unavailable")
	}
	ref, config := functionBootstrapFixture(t, scope)
	ref.Namespace = k.namespace
	key := bytes.Repeat([]byte{21}, 32)
	loss := &functionLiveLostSecretReply{delegate: k.http.Transport}
	k.http.Transport = loss
	original, err := k.reserveFunctionBootstrap(ctx, key, ref, config)
	if err != nil || original.Reference.UID == "" || !loss.lost {
		t.Fatal("actual accepted bootstrap did not recover its lost reply")
	}
	restarted, err := newKubeClient()
	if err != nil {
		t.Fatal("fresh projected identity unavailable")
	}
	// The caller no longer has a receipt or generated leaf/key. A GET resolves
	// the original bootstrap before any new random material can be issued.
	again, exists, err := restarted.findFunctionBootstrap(ctx, key, ref)
	if err != nil || !exists || again.Reference != original.Reference || again.Config.ManagerPrivateKey != config.ManagerPrivateKey || again.Config.ArtifactKey != config.ArtifactKey {
		t.Fatal("actual bootstrap identity changed on restart")
	}
	if _, err = restarted.readFunctionBootstrap(ctx, key, original.Reference); err != nil {
		t.Fatal("actual persisted bootstrap unreadable")
	}
	wrong := original.Reference
	wrong.UID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if _, err = restarted.readFunctionBootstrap(ctx, key, wrong); err == nil {
		t.Fatal("replacement UID adopted")
	}
	wrong = original.Reference
	wrong.Scope.BranchID = "br_" + randomToken(8)
	if _, err = restarted.readFunctionBootstrap(ctx, key, wrong); err == nil {
		t.Fatal("foreign branch adopted")
	}
	changed := config
	changed.Environment = map[string]string{"GREETING": "changed"}
	if _, err = restarted.reserveFunctionBootstrap(ctx, key, ref, changed); err == nil {
		t.Fatal("retained bootstrap overwritten or adopted for different content")
	}
	t.Logf("retained bootstrap namespace=%s name=%s uid=%s instance=%s deployment=%s", ref.Namespace, ref.Name, original.Reference.UID, scope.InstanceID, scope.DeploymentID)
}
