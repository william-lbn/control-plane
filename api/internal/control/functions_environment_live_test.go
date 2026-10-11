//go:build live

package control

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

// This transport injects a lost acknowledgement only AFTER the real API server
// has accepted the create. The recovery GET still reaches that actual server.
// It does not emulate Secret storage or Kubernetes ownership/immutability.
type functionLiveLostSecretReply struct {
	delegate http.RoundTripper
	mu       sync.Mutex
	lost     bool
}

func (r *functionLiveLostSecretReply) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := r.delegate.RoundTrip(request)
	if err != nil {
		return response, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.lost && request.Method == http.MethodPost && response.StatusCode == http.StatusCreated {
		r.lost = true
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return nil, errors.New("controlled post-commit acknowledgement loss")
	}
	return response, nil
}

func TestFunctionEnvironmentLiveKubernetes(t *testing.T) {
	if os.Getenv("NEON_FUNCTIONS_LIVE_ENVIRONMENT_TEST") != "true" {
		t.Fatal("explicit Linux live environment admission is required")
	}
	deployment := os.Getenv("NEON_FUNCTIONS_LIVE_DEPLOYMENT_ID")
	if !functionDeploymentID.MatchString(deployment) {
		t.Fatal("explicit owned native deployment test identity required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	k, err := newKubeClient()
	if err != nil {
		t.Fatal("dedicated projected test service account unavailable")
	}
	loss := &functionLiveLostSecretReply{delegate: k.http.Transport}
	k.http.Transport = loss
	// This account can create Secrets and GET this exact name only. It has no
	// list, patch, delete, VM, Pod or metadata rights. Values are disposable and
	// never printed; the immutable Secret remains evidence with its actual UID.
	key := bytes.Repeat([]byte{19}, 32)
	project, branch := "prj_"+randomToken(8), "br_"+randomToken(8)
	environment := map[string]string{"GREETING": "hello", "TEST_TOKEN": randomToken(16)}
	ref, err := k.reserveFunctionEnvironment(ctx, key, project, branch, deployment, environment)
	if err != nil || ref.UID == "" || !loss.lost {
		t.Fatal("real committed Secret did not recover its lost reply")
	}
	restarted, err := newKubeClient()
	if err != nil {
		t.Fatal("fresh projected test client unavailable")
	}
	replay, err := restarted.reserveFunctionEnvironment(ctx, key, project, branch, deployment, environment)
	if err != nil || replay != ref {
		t.Fatal("actual immutable environment did not replay after restart")
	}
	values, err := restarted.readFunctionEnvironment(ctx, key, ref)
	if err != nil || !reflect.DeepEqual(values, environment) {
		t.Fatal("actual canonical Secret content not verified")
	}
	wrong := ref
	wrong.UID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if _, err = restarted.readFunctionEnvironment(ctx, key, wrong); err == nil {
		t.Fatal("replacement UID adopted")
	}
	wrong = ref
	wrong.BranchID = "br_" + randomToken(8)
	if _, err = restarted.readFunctionEnvironment(ctx, key, wrong); err == nil {
		t.Fatal("foreign branch environment adopted")
	}
	if _, err = restarted.reserveFunctionEnvironment(ctx, key, project, branch, deployment, map[string]string{"GREETING": "changed"}); err == nil {
		t.Fatal("actual retained Secret was adopted for changed values")
	}
	t.Logf("retained environment namespace=%s name=%s uid=%s project=%s branch=%s", ref.Namespace, ref.Name, ref.UID, ref.ProjectID, ref.BranchID)
}
