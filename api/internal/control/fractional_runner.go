package control

import (
	"errors"
	"strings"
)

func needsFractionalRunner(vm map[string]any) bool {
	return cpuMilli(nested(vm, "spec", "guest", "cpus", "min"))%1000 != 0 || cpuMilli(nested(vm, "spec", "guest", "cpus", "max"))%1000 != 0
}

// A VM's Running phase and /cpu_current can both be true in dummy mode.
// Do not publish a new fractional Endpoint whose runner explicitly skips the
// limiter. Kernel quota convergence is independently checked by operator E2E.
func validateFractionalRunnerPod(vm, pod map[string]any) error {
	if !needsFractionalRunner(vm) {
		return nil
	}
	reject := errors.New("fractional CPU requires an owned runner with enabled cgroup management and writable host cgroup mount")
	uid := stringVal(nested(vm, "metadata", "uid"))
	if uid == "" || nested(pod, "metadata", "name") != nested(vm, "status", "podName") {
		return reject
	}
	refs, _ := nested(pod, "metadata", "ownerReferences").([]any)
	owned := false
	for _, value := range refs {
		ref, _ := value.(map[string]any)
		if ref["uid"] == uid && ref["kind"] == "VirtualMachine" {
			owned = true
		}
	}
	if !owned {
		return reject
	}
	containers, _ := nested(pod, "spec", "containers").([]any)
	for _, value := range containers {
		container, _ := value.(map[string]any)
		if container["name"] != "neonvm-runner" {
			continue
		}
		for _, key := range []string{"command", "args"} {
			args, _ := container[key].([]any)
			for _, v := range args {
				arg := strings.TrimLeft(stringVal(v), "-")
				if strings.HasPrefix(arg, "skip-cgroup-management") && arg != "skip-cgroup-management=false" {
					return reject
				}
			}
		}
		mounts, _ := container["volumeMounts"].([]any)
		for _, v := range mounts {
			mount, _ := v.(map[string]any)
			if mount["mountPath"] != "/sys/fs/cgroup" || mount["readOnly"] == true {
				continue
			}
			volumes, _ := nested(pod, "spec", "volumes").([]any)
			for _, v := range volumes {
				volume, _ := v.(map[string]any)
				if volume["name"] == mount["name"] && nested(volume, "hostPath", "path") == "/sys/fs/cgroup" {
					return nil
				}
			}
		}
		return reject
	}
	return reject
}
