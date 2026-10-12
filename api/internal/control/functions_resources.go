package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

// Private leased Driver input. Image/node placement come from platform policy,
// never a customer deployment. The persisted bootstrap must be re-read and
// MAC/UID verified before any runtime resource request is issued.
type functionRuntimePlan struct {
	Target    functionInstanceTarget
	Instance  functionInstanceRecord
	Bootstrap functionBootstrapReference
	Image     string
	NodeName  string
}

type functionRuntimeResource struct {
	UID       string
	ClusterIP string
}

var functionVMImage = regexp.MustCompile(`^[a-z0-9./-]+@sha256:[a-f0-9]{64}$`)
var functionNodeName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
var errFunctionRuntimeResource = errors.New("Functions runtime resource unavailable or ownership rejected")

func (p functionRuntimePlan) valid(namespace string) bool {
	i, b := p.Instance, p.Bootstrap
	name := "fn-" + strings.TrimPrefix(i.Scope.InstanceID, "fni_")
	return p.Target.valid() && i.Scope.Validate() == nil && b.valid() && b.UID != "" && b.Namespace == namespace &&
		b.Scope == i.Scope && b.FunctionID == p.Target.FunctionID && b.Scope.ProjectID == p.Target.ProjectID &&
		b.Scope.BranchID == p.Target.BranchID && b.Scope.DeploymentID == p.Target.DeploymentID && b.Scope.Generation == p.Target.Generation && b.Scope.Slug == p.Target.Slug &&
		i.VMName == name && i.ServiceName == name && i.SecretName == b.Name && (i.State == "provisioning" || i.State == "starting") &&
		(i.VMUID == nil || functionResourceUID.MatchString(*i.VMUID)) && (i.ServiceUID == nil || functionResourceUID.MatchString(*i.ServiceUID)) &&
		functionVMImage.MatchString(p.Image) && (p.NodeName == "" || functionNodeName.MatchString(p.NodeName))
}

func functionRuntimeBody(p functionRuntimePlan, kind string) map[string]any {
	labels := functionBootstrapLabels(p.Bootstrap)
	labels["neon-control/backing-kind"] = "function-runtime-v1"
	metadata := map[string]any{"namespace": p.Bootstrap.Namespace, "name": p.Instance.VMName, "labels": labels,
		"annotations": map[string]string{"neon-control/bootstrap-uid": p.Bootstrap.UID}}
	if kind == "service" {
		return map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": metadata,
			"spec": map[string]any{"selector": map[string]string{"vm.neon.tech/name": p.Instance.VMName}, "ports": []any{map[string]any{"name": "manager", "port": 9090, "targetPort": "manager", "protocol": "TCP"}}}}
	}
	spec := map[string]any{"schedulerName": "autoscale-scheduler", "restartPolicy": "Never", "enableSSH": false, "enableAcceleration": true, "enableNetworkMonitoring": false, "service_links": false,
		// NeonVM fills this immutable field during reconciliation if omitted.
		// Declare the own-fork mode explicitly so post-controller GET verification
		// checks the same complete spec; never ignore a foreign scaling mode.
		"cpuScalingMode": "QmpScaling",
		// The own-fork typed admission webhook serializes this empty struct.
		// Bind the exact empty value instead of accepting arbitrary overrides.
		"podResources": map[string]any{},
		"qmp":          20183, "qmpManual": 20184, "runnerPort": 25183, "terminationGracePeriodSeconds": 5,
		"guest": map[string]any{"cpus": map[string]any{"min": 1, "use": 1, "max": 1}, "memorySlotSize": "1Gi", "memorySlots": map[string]any{"min": 2, "use": 2, "max": 2},
			"rootDisk": map[string]any{"image": p.Image, "imagePullPolicy": "IfNotPresent", "size": "2Gi"}, "ports": []any{map[string]any{"name": "manager", "port": 9090, "protocol": "TCP"}}},
		"extraNetwork": map[string]any{"enable": false, "interface": "net1"},
		"disks": []any{map[string]any{"name": "function-bootstrap", "mountPath": "/run/function-bootstrap", "readOnly": true, "watch": false,
			"secret": map[string]any{"secretName": p.Bootstrap.Name, "defaultMode": 0400, "items": []any{map[string]any{"key": "config.json", "path": "config.json", "mode": 0400}}}}}}
	if p.NodeName != "" {
		spec["nodeSelector"] = map[string]string{"kubernetes.io/hostname": p.NodeName}
	}
	return map[string]any{"apiVersion": "vm.neon.tech/v1", "kind": "VirtualMachine", "metadata": metadata, "spec": spec}
}

// Compare the complete execution spec, including absence of overrides/disks/
// env/runner image. Unknown fields fail closed. Service-assigned IPv4 fields
// and exact harmless API defaults are the only exclusions. Labels/annotations
// are additional ownership evidence, never a substitute for spec verification.
func verifyFunctionRuntimeResource(item map[string]any, p functionRuntimePlan, kind string) (functionRuntimeResource, error) {
	var empty functionRuntimeResource
	expected := functionRuntimeBody(p, kind)
	if item["apiVersion"] != expected["apiVersion"] || item["kind"] != expected["kind"] ||
		stringVal(nested(item, "metadata", "namespace")) != p.Bootstrap.Namespace || stringVal(nested(item, "metadata", "name")) != p.Instance.VMName || nested(item, "metadata", "deletionTimestamp") != nil {
		return empty, errFunctionRuntimeResource
	}
	for name, value := range expected["metadata"].(map[string]any)["labels"].(map[string]string) {
		if stringVal(nested(item, "metadata", "labels", name)) != value {
			return empty, errFunctionRuntimeResource
		}
	}
	if stringVal(nested(item, "metadata", "annotations", "neon-control/bootstrap-uid")) != p.Bootstrap.UID {
		return empty, errFunctionRuntimeResource
	}
	uid := stringVal(nested(item, "metadata", "uid"))
	known := p.Instance.VMUID
	if kind == "service" {
		known = p.Instance.ServiceUID
	}
	if !functionResourceUID.MatchString(uid) || (known != nil && *known != uid) {
		return empty, errFunctionRuntimeResource
	}
	raw, err := json.Marshal(item["spec"])
	if err != nil {
		return empty, errFunctionRuntimeResource
	}
	var spec map[string]any
	if json.Unmarshal(raw, &spec) != nil || spec == nil {
		return empty, errFunctionRuntimeResource
	}
	result := functionRuntimeResource{UID: uid}
	if kind == "service" {
		ip, err := netip.ParseAddr(stringVal(spec["clusterIP"]))
		if err != nil || !ip.Is4() || !ip.IsPrivate() {
			return empty, errFunctionRuntimeResource
		}
		result.ClusterIP = ip.String()
		// Require one assigned ClusterIP, no NodePort/external ingress or IPv6.
		defaults := map[string]any{"clusterIP": ip.String(), "clusterIPs": []any{ip.String()}, "type": "ClusterIP", "sessionAffinity": "None", "ipFamilyPolicy": "SingleStack", "ipFamilies": []any{"IPv4"}, "internalTrafficPolicy": "Cluster"}
		for name, value := range defaults {
			if actual, ok := spec[name]; ok {
				a, _ := json.Marshal(actual)
				b, _ := json.Marshal(value)
				if !bytes.Equal(a, b) {
					return empty, errFunctionRuntimeResource
				}
				delete(spec, name)
			}
		}
	}
	actual, _ := json.Marshal(spec)
	want, _ := json.Marshal(expected["spec"])
	if !bytes.Equal(actual, want) {
		return empty, errFunctionRuntimeResource
	}
	return result, nil
}

// Reserve one exact Service/VM. POST is sent at most once; an unknown reply is
// resolved by GET the original name and full spec/UID verification. Persisted
// resource loss cannot authorize same-name replacement. No patch, deletion,
// customer command override, readiness claim or publication occurs here.
// Metadata lease validity and external admission fencing remain caller duties.
func (k *kubeClient) reserveFunctionRuntimeResource(ctx context.Context, key []byte, p functionRuntimePlan, kind string) (functionRuntimeResource, error) {
	var empty functionRuntimeResource
	if k == nil || (kind != "service" && kind != "vm") || !p.valid(k.namespace) {
		return empty, errFunctionRuntimeResource
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := k.readFunctionBootstrap(ctx, key, p.Bootstrap); err != nil {
		return empty, errFunctionRuntimeResource
	}
	item, err := k.request(ctx, http.MethodGet, k.path(kind, p.Instance.VMName), nil)
	if err == nil {
		return verifyFunctionRuntimeResource(item, p, kind)
	}
	var ke kubeError
	known := p.Instance.VMUID
	if kind == "service" {
		known = p.Instance.ServiceUID
	}
	if !errors.As(err, &ke) || ke.Status != http.StatusNotFound || known != nil {
		return empty, errFunctionRuntimeResource
	}
	item, err = k.request(ctx, http.MethodPost, k.path(kind, ""), functionRuntimeBody(p, kind))
	if err != nil {
		item, err = k.request(ctx, http.MethodGet, k.path(kind, p.Instance.VMName), nil)
	}
	if err != nil {
		return empty, errFunctionRuntimeResource
	}
	return verifyFunctionRuntimeResource(item, p, kind)
}
