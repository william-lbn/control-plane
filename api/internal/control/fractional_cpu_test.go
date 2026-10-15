package control

import (
	"encoding/json"
	"testing"
)

func TestFractionalCPUBoundsAndQuantitySerialization(t *testing.T) {
	for _, b := range []createBounds{{250, 500, 1024, 2048}, {250, 750, 1024, 3072}, {500, 500, 2048, 2048}, {1000, 2000, 1024, 3072}} {
		if !b.valid() {
			t.Fatal("supported profile rejected", b)
		}
	}
	for _, b := range []createBounds{{0, 500, 1024, 2048}, {250, 500, 2048, 2048}, {500, 1000, 2048, 3072}, {251, 500, 1024, 2048}, {250, 500, 1024, 4096}} {
		if b.valid() {
			t.Fatal("unverified CU profile admitted", b)
		}
	}
	p := boundsPayload{ProjectID: "prj-test", EndpointID: "ep_test", WorkloadName: "cp-test", MinCPU: 250, MaxCPU: 500, MinMem: 1024, MaxMem: 2048}
	raw, e := boundsAnnotation(p)
	if e != nil {
		t.Fatal(e)
	}
	var doc map[string]any
	json.Unmarshal([]byte(raw), &doc)
	if nested(doc, "min", "cpu") != "250m" || nested(doc, "max", "cpu") != "500m" {
		t.Fatal("fractional bound truncated")
	}
	rawBytes, _ := json.Marshal(map[string]any{"min": vmCPUValue(250), "max": vmCPUValue(1000)})
	json.Unmarshal(rawBytes, &doc)
	if cpuMilli(doc["min"]) != 250 || cpuMilli(doc["max"]) != 1000 {
		t.Fatal("VM quantity serialization changed effective CPU")
	}
}
func TestFractionalCPUResumeBoundsPreserveCapacityAndRejectOldVMFloor(t *testing.T) {
	k, f, p := newBoundsFixture()
	p.MinCPU = 250
	p.MaxCPU = 500
	p.MinMem = 1024
	p.MaxMem = 2048
	guest := nested(f.vm, "spec", "guest").(map[string]any)
	guest["cpus"] = map[string]any{"min": "250m", "max": "500m", "use": "250m"}
	guest["memorySlots"] = map[string]any{"min": 1, "max": 2, "use": 1}
	routes := f.routes()
	route := routes[selector(p.EndpointID)].(map[string]any)
	route["vm_template"] = cloneBoundsObject(f.vm)
	f.storeRoutes(routes)
	if e := k.reconcileVMBounds(t.Context(), p); e != nil {
		t.Fatal(e)
	}
	annotation, e := boundsAnnotation(p)
	if e != nil || nested(f.vm, "metadata", "annotations", autoscalingBoundsAnnotation) != annotation {
		t.Fatal("live fractional bounds absent", e)
	}
	if nested(f.vm, "spec", "guest", "cpus", "min") != "250m" {
		t.Fatal("immutable minimum changed")
	}
	guest["cpus"] = map[string]any{"min": 1, "max": 2, "use": 1}
	if e := validateBoundsVM(f.vm, p, false); e == nil {
		t.Fatal("old 1CPU VM admitted lower immutable floor")
	}
}
