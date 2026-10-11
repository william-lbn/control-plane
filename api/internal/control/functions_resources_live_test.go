//go:build live

package control

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

// Actual ClusterIP creation/recovery plus API-server dry-run CRD admission.
// No VM is persisted or run, no readiness/SQL/metadata publication is claimed.
// Scope/Secret/Service are retained; the operator archives their actual UIDs.
func TestFunctionRuntimeServiceLiveKubernetes(t *testing.T) {
	if os.Getenv("NEON_FUNCTIONS_LIVE_RUNTIME_TEST") != "true" {
		t.Fatal("explicit runtime backing admission required")
	}
	scope := functions.Scope{InstanceID: os.Getenv("NEON_FUNCTIONS_LIVE_INSTANCE_ID"), DeploymentID: os.Getenv("NEON_FUNCTIONS_LIVE_DEPLOYMENT_ID"), ProjectID: "prj_" + randomToken(8), BranchID: "br_" + randomToken(8), Slug: "hello", Generation: 1}
	if scope.Validate() != nil {
		t.Fatal("explicit owned runtime identity required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	k, err := newKubeClient()
	if err != nil {
		t.Fatal("dedicated projected identity unavailable")
	}
	ref, config := functionBootstrapFixture(t, scope)
	ref.Namespace = k.namespace
	key := bytes.Repeat([]byte{35}, 32)
	original, err := k.reserveFunctionBootstrap(ctx, key, ref, config)
	if err != nil {
		t.Fatal("actual bootstrap unavailable")
	}
	ref = original.Reference
	name := "fn-" + strings.TrimPrefix(scope.InstanceID, "fni_")
	p := functionRuntimePlan{Target: functionInstanceTarget{ref.FunctionID, scope.ProjectID, scope.BranchID, scope.DeploymentID, scope.Generation, scope.Slug},
		Instance: functionInstanceRecord{Scope: scope, VMName: name, ServiceName: name, SecretName: ref.Name, State: "provisioning"}, Bootstrap: ref,
		Image: os.Getenv("NEON_FUNCTIONS_LIVE_VM_IMAGE")}
	loss := &functionLiveLostSecretReply{delegate: k.http.Transport}
	// The transport deliberately loses the one accepted Service reply. It also
	// sees no bootstrap POST here: the immutable Secret is already persisted.
	k.http.Transport = loss
	service, err := k.reserveFunctionRuntimeResource(ctx, key, p, "service")
	if err != nil || service.UID == "" || service.ClusterIP == "" || !loss.lost {
		t.Fatal("actual Service lost reply did not recover original UID", err)
	}
	p.Instance.ServiceUID = &service.UID
	restarted, err := newKubeClient()
	if err != nil {
		t.Fatal("restarted projected client unavailable")
	}
	again, err := restarted.reserveFunctionRuntimeResource(ctx, key, p, "service")
	if err != nil || again != service {
		t.Fatal("actual Service identity changed on restart", err)
	}
	wrong := p
	foreign := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	wrong.Instance.ServiceUID = &foreign
	if _, err = restarted.reserveFunctionRuntimeResource(ctx, key, wrong, "service"); err == nil {
		t.Fatal("foreign Service UID adopted")
	}
	// Dry-run performs the real CRD/webhook/default validation while creating
	// zero guest VMs. Never treat this generated dry-run UID as a durable receipt.
	admitted, err := restarted.request(ctx, http.MethodPost, restarted.path("vm", "")+"?dryRun=All", functionRuntimeBody(p, "vm"))
	if err != nil {
		t.Fatal("actual Functions CRD dry-run admission rejected")
	}
	if _, err = verifyFunctionRuntimeResource(admitted, p, "vm"); err != nil {
		t.Fatal("actual CRD defaults violated immutable runtime contract")
	}
	t.Logf("retained runtime namespace=%s service=%s uid=%s bootstrap=%s uid=%s vm_persisted=false", k.namespace, name, service.UID, ref.Name, ref.UID)
}
