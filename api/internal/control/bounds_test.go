package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// A local RoundTripper models Kubernetes resourceVersion conflicts without a
// real API server, database, network listener or changes to the test cluster.
type boundsAPI struct {
	vm, secret                        map[string]any
	secretConflicts, vmConflicts      int
	secretPuts, vmPatches             int
	failNextPatch                     bool
	concurrentSecretChange, recreated bool
	changeTemplateOnVerification      bool
}

func cloneBoundsObject(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var clone map[string]any
	_ = json.Unmarshal(b, &clone)
	return clone
}

func (f *boundsAPI) response(status int, value any) (*http.Response, error) {
	b, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(string(b)))}, nil
}

func (f *boundsAPI) routes() map[string]any {
	raw, _ := secretText(f.secret, "routes.json")
	var routes map[string]any
	_ = json.Unmarshal([]byte(raw), &routes)
	return routes
}

func (f *boundsAPI) storeRoutes(routes map[string]any) {
	b, _ := json.Marshal(routes)
	f.secret["data"].(map[string]any)["routes.json"] = base64.StdEncoding.EncodeToString(b)
	rv, _ := strconv.Atoi(stringVal(nested(f.secret, "metadata", "resourceVersion")))
	f.secret["metadata"].(map[string]any)["resourceVersion"] = strconv.Itoa(rv + 1)
}

func (f *boundsAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	secret := strings.Contains(r.URL.Path, "/secrets/")
	if r.Method == http.MethodGet {
		if secret {
			if f.changeTemplateOnVerification && f.secretPuts > 0 && f.vmPatches > 0 {
				routes := f.routes()
				nested(routes, "ep-test", "vm_template", "metadata", "annotations").(map[string]any)[autoscalingBoundsAnnotation] = "later-concurrent-bounds"
				f.storeRoutes(routes)
				f.changeTemplateOnVerification = false
			}
			return f.response(200, f.secret)
		}
		if f.vm == nil {
			return f.response(404, map[string]string{"message": "not found"})
		}
		return f.response(200, f.vm)
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	if secret && r.Method == http.MethodPut {
		f.secretPuts++
		if f.secretConflicts != 0 {
			if f.secretConflicts > 0 {
				f.secretConflicts--
			}
			if !f.concurrentSecretChange {
				routes := f.routes()
				routes["ep-other"] = map[string]any{"lifecycle_generation": "must-survive", "verifier": "other-route"}
				target := routes["ep-test"].(map[string]any)
				target["wake_generation"] = "concurrent-wake"
				nested(target, "vm_template", "metadata", "annotations").(map[string]any)["other/annotation"] = "concurrent-template"
				f.storeRoutes(routes)
				f.concurrentSecretChange = true
			}
			return f.response(409, map[string]string{"message": "conflict"})
		}
		if nested(body, "metadata", "resourceVersion") != nested(f.secret, "metadata", "resourceVersion") {
			return f.response(409, map[string]string{"message": "stale resourceVersion"})
		}
		f.secret = body
		rv, _ := strconv.Atoi(stringVal(nested(body, "metadata", "resourceVersion")))
		f.secret["metadata"].(map[string]any)["resourceVersion"] = strconv.Itoa(rv + 1)
		return f.response(200, f.secret)
	}
	if !secret && r.Method == http.MethodPatch {
		f.vmPatches++
		if f.failNextPatch {
			f.failNextPatch = false
			return f.response(503, map[string]string{"message": "temporary API error"})
		}
		if f.vmConflicts != 0 {
			if f.vmConflicts > 0 {
				f.vmConflicts--
			}
			f.vm["metadata"].(map[string]any)["uid"] = "new-wake-uid"
			f.vm["metadata"].(map[string]any)["resourceVersion"] = "9"
			f.vm["metadata"].(map[string]any)["annotations"].(map[string]any)["other/annotation"] = "new-wake"
			f.recreated = true
			return f.response(409, map[string]string{"message": "conflict"})
		}
		if nested(body, "metadata", "resourceVersion") != nested(f.vm, "metadata", "resourceVersion") ||
			nested(body, "metadata", "uid") != nested(f.vm, "metadata", "uid") {
			return f.response(409, map[string]string{"message": "stale identity/version"})
		}
		annotations := f.vm["metadata"].(map[string]any)["annotations"].(map[string]any)
		for key, value := range nested(body, "metadata", "annotations").(map[string]any) {
			annotations[key] = value
		}
		return f.response(200, f.vm)
	}
	return nil, fmt.Errorf("unexpected Kubernetes request: %s %s", r.Method, r.URL.Path)
}

func newBoundsFixture() (*kubeClient, *boundsAPI, boundsPayload) {
	p := boundsPayload{ProjectID: "prj-test", EndpointID: "ep_test", WorkloadName: "cp-test",
		MinCPU: 2000, MaxCPU: 2000, MinMem: 2048, MaxMem: 3072}
	vm := map[string]any{"apiVersion": "vm.neon.tech/v1", "kind": "VirtualMachine",
		"metadata": map[string]any{"name": p.WorkloadName, "uid": "original-uid", "resourceVersion": "1",
			"labels":      map[string]any{"neon-control/project-id": p.ProjectID, "neon-control/endpoint-id": p.EndpointID},
			"annotations": map[string]any{autoscalingBoundsAnnotation: "old-bounds", "other/annotation": "keep"}},
		"spec": map[string]any{"guest": map[string]any{"cpus": map[string]any{"min": 1, "max": 2, "use": 1},
			"memorySlots": map[string]any{"min": 1, "max": 3, "use": 1}, "memorySlotSize": "1Gi",
			"rootDisk": map[string]any{"image": defaultComputeImage}, "args": []any{"immutable-arg"}}}}
	template := cloneBoundsObject(vm)
	delete(template["metadata"].(map[string]any), "uid")
	delete(template["metadata"].(map[string]any), "resourceVersion")
	routes := map[string]any{selector(p.EndpointID): map[string]any{"project_id": p.ProjectID,
		"kind": "neonvm", "workload": p.WorkloadName, "roles": map[string]any{"cloud_admin": "must-survive"},
		"vm_template": template}}
	encoded, _ := json.Marshal(routes)
	f := &boundsAPI{vm: cloneBoundsObject(vm), secret: map[string]any{"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": routesSecret, "resourceVersion": "1", "annotations": map[string]any{"keep": "secret-meta"}},
		"data":     map[string]any{"routes.json": base64.StdEncoding.EncodeToString(encoded), "unrelated-key": "keep-me"}}}
	k := &kubeClient{base: "https://kubernetes.invalid", namespace: "neon", http: &http.Client{Transport: f}}
	return k, f, p
}

func TestBoundsResumePersistenceAndIdempotentReplay(t *testing.T) {
	k, f, p := newBoundsFixture()
	beforeSpec := cloneBoundsObject(f.vm["spec"].(map[string]any))
	if err := k.reconcileVMBounds(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	wanted, _ := boundsAnnotation(p)
	template := nested(f.routes(), selector(p.EndpointID), "vm_template").(map[string]any)
	if nested(template, "metadata", "annotations", autoscalingBoundsAnnotation) != wanted ||
		nested(f.vm, "metadata", "annotations", autoscalingBoundsAnnotation) != wanted {
		t.Fatal("live and cold-wake bounds did not converge")
	}
	// Model suspend followed by adapter POST of the persisted template. Hard
	// VM limits, fork image and guest arguments must survive the soft update.
	f.vm = cloneBoundsObject(template)
	f.vm["metadata"].(map[string]any)["uid"] = "resumed-uid"
	f.vm["metadata"].(map[string]any)["resourceVersion"] = "10"
	if !reflect.DeepEqual(f.vm["spec"], beforeSpec) {
		t.Fatal("soft bounds update changed immutable VM configuration")
	}
	puts, patches := f.secretPuts, f.vmPatches
	if err := k.reconcileVMBounds(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if f.secretPuts != puts || f.vmPatches != patches {
		t.Fatal("replay wrote resources already at desired bounds")
	}
}

func TestBoundsCASConflictPreservesConcurrentRoutesAndWake(t *testing.T) {
	k, f, p := newBoundsFixture()
	f.secretConflicts, f.vmConflicts = 1, 1
	if err := k.reconcileVMBounds(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	routes := f.routes()
	if nested(routes, "ep-other", "lifecycle_generation") != "must-survive" ||
		nested(routes, selector(p.EndpointID), "wake_generation") != "concurrent-wake" ||
		nested(routes, selector(p.EndpointID), "vm_template", "metadata", "annotations", "other/annotation") != "concurrent-template" ||
		nested(routes, selector(p.EndpointID), "roles", "cloud_admin") != "must-survive" ||
		nested(f.secret, "data", "unrelated-key") != "keep-me" {
		t.Fatal("CAS retry lost concurrent route, template or Secret fields")
	}
	if nested(f.vm, "metadata", "uid") != "new-wake-uid" ||
		nested(f.vm, "metadata", "annotations", "other/annotation") != "new-wake" ||
		f.secretPuts != 2 || f.vmPatches != 2 {
		t.Fatal("live VM CAS did not reread the concurrently recreated VM")
	}
}

func TestBoundsPartialFailureCanResumeWithoutFalseSuccess(t *testing.T) {
	k, f, p := newBoundsFixture()
	f.failNextPatch = true
	if err := k.reconcileVMBounds(context.Background(), p); !kubeStatusIs(err, 503) {
		t.Fatalf("live patch failure was not propagated: %v", err)
	}
	wanted, _ := boundsAnnotation(p)
	if nested(f.routes(), selector(p.EndpointID), "vm_template", "metadata", "annotations", autoscalingBoundsAnnotation) != wanted ||
		nested(f.vm, "metadata", "annotations", autoscalingBoundsAnnotation) == wanted {
		t.Fatal("partial failure fixture did not retain durable resume state")
	}
	if err := k.reconcileVMBounds(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if f.secretPuts != 1 || nested(f.vm, "metadata", "annotations", autoscalingBoundsAnnotation) != wanted {
		t.Fatal("recovery did not converge idempotently")
	}
}

func TestBoundsSuspendedEndpointUpdatesWithoutWake(t *testing.T) {
	k, f, p := newBoundsFixture()
	f.vm = nil
	if err := k.reconcileVMBounds(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	wanted, _ := boundsAnnotation(p)
	if f.vm != nil || f.vmPatches != 0 ||
		nested(f.routes(), selector(p.EndpointID), "vm_template", "metadata", "annotations", autoscalingBoundsAnnotation) != wanted {
		t.Fatal("suspended endpoint was woken or resume bounds were not persisted")
	}
}

func TestBoundsDeletingVMUpdatesNextWakeTemplate(t *testing.T) {
	k, f, p := newBoundsFixture()
	f.vm["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-03T00:00:00Z"
	if err := k.reconcileVMBounds(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	wanted, _ := boundsAnnotation(p)
	if f.vmPatches != 0 || nested(f.routes(), selector(p.EndpointID), "vm_template", "metadata", "annotations", autoscalingBoundsAnnotation) != wanted {
		t.Fatal("bounds changed a deleting VM or failed to update its next wake")
	}
}

func TestBoundsFinalVerificationRejectsConcurrentTemplateReplacement(t *testing.T) {
	k, f, p := newBoundsFixture()
	f.changeTemplateOnVerification = true
	if err := k.reconcileVMBounds(context.Background(), p); err == nil {
		t.Fatal("bounds success reported after final resume template lost desired value")
	}
	if nested(f.routes(), selector(p.EndpointID), "vm_template", "metadata", "annotations", autoscalingBoundsAnnotation) != "later-concurrent-bounds" {
		t.Fatal("final verification overwrote a concurrent replacement")
	}
}

func TestBoundsConflictExhaustionAndOwnershipFailBeforeLiveMutation(t *testing.T) {
	for _, scenario := range []string{"conflict", "route-owner", "template-owner", "live-owner", "capacity", "invalid-identity"} {
		t.Run(scenario, func(t *testing.T) {
			k, f, p := newBoundsFixture()
			routes := f.routes()
			switch scenario {
			case "conflict":
				f.secretConflicts = -1
			case "route-owner":
				routes[selector(p.EndpointID)].(map[string]any)["project_id"] = "another-project"
				f.storeRoutes(routes)
			case "template-owner":
				nested(routes, selector(p.EndpointID), "vm_template", "metadata", "labels").(map[string]any)["neon-control/endpoint-id"] = "another-endpoint"
				f.storeRoutes(routes)
			case "live-owner":
				f.vm["metadata"].(map[string]any)["labels"].(map[string]any)["neon-control/project-id"] = "another-project"
			case "capacity":
				nested(routes, selector(p.EndpointID), "vm_template", "spec", "guest", "cpus").(map[string]any)["max"] = 1
				f.storeRoutes(routes)
			case "invalid-identity":
				p.WorkloadName = "other-vm"
			}
			if err := k.reconcileVMBounds(context.Background(), p); err == nil {
				t.Fatal("unsafe or uncompleted bounds update was reported successful")
			}
			if f.vmPatches != 0 || (scenario != "conflict" && f.secretPuts != 0) {
				t.Fatal("validation failure mutated an unrelated or invalid resource")
			}
			if scenario == "conflict" && f.secretPuts != 6 {
				t.Fatal("persistent conflict was not bounded")
			}
		})
	}
}
