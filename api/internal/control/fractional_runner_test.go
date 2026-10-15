package control

import "testing"

func TestFractionalRunnerRejectsDummyAndForeignPod(t *testing.T) {
	vm := map[string]any{"metadata": map[string]any{"uid": "vm-original"}, "spec": map[string]any{"guest": map[string]any{"cpus": map[string]any{"min": "250m", "max": "500m"}}}, "status": map[string]any{"podName": "owned-runner"}}
	pod := map[string]any{"metadata": map[string]any{"name": "owned-runner", "ownerReferences": []any{map[string]any{"uid": "vm-original", "kind": "VirtualMachine"}}}, "spec": map[string]any{"containers": []any{map[string]any{"name": "neonvm-runner", "command": []any{"runner", "-skip-cgroup-management"}}}}}
	if validateFractionalRunnerPod(vm, pod) == nil {
		t.Fatal("dummy fractional runner admitted")
	}
	container := nested(pod, "spec", "containers").([]any)[0].(map[string]any)
	container["command"] = []any{"runner"}
	container["volumeMounts"] = []any{map[string]any{"name": "cgroup", "mountPath": "/sys/fs/cgroup"}}
	pod["spec"].(map[string]any)["volumes"] = []any{map[string]any{"name": "cgroup", "hostPath": map[string]any{"path": "/sys/fs/cgroup"}}}
	if err := validateFractionalRunnerPod(vm, pod); err != nil {
		t.Fatal(err)
	}
	container["volumeMounts"].([]any)[0].(map[string]any)["readOnly"] = true
	if validateFractionalRunnerPod(vm, pod) == nil {
		t.Fatal("read-only mount admitted")
	}
	container["volumeMounts"].([]any)[0].(map[string]any)["readOnly"] = false
	pod["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"uid": "replacement-vm", "kind": "VirtualMachine"}}
	if validateFractionalRunnerPod(vm, pod) == nil {
		t.Fatal("foreign owner admitted")
	}
	vm["spec"].(map[string]any)["guest"].(map[string]any)["cpus"] = map[string]any{"min": 1, "max": 2}
	if err := validateFractionalRunnerPod(vm, nil); err != nil {
		t.Fatal("legacy integer admission changed")
	}
}
