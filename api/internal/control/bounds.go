package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

const autoscalingBoundsAnnotation = "autoscaling.neon.tech/bounds"

func kubeStatusIs(err error, status int) bool {
	var kubeErr kubeError
	return errors.As(err, &kubeErr) && kubeErr.Status == status
}

func boundsAnnotation(p boundsPayload) (string, error) {
	if p.ProjectID == "" || p.EndpointID == "" || p.WorkloadName != kubeName(p.EndpointID) {
		return "", errors.New("bounds workload identity mismatch")
	}
	if p.MinCPU > p.MaxCPU || p.MinMem > p.MaxMem || p.MinCPU < 1000 || p.MaxCPU > 2000 ||
		p.MinMem < 1024 || p.MaxMem > 3072 || p.MinCPU%1000 != 0 || p.MaxCPU%1000 != 0 ||
		p.MinMem%1024 != 0 || p.MaxMem%1024 != 0 {
		return "", errors.New("unsupported bounds")
	}
	b, err := json.Marshal(map[string]any{
		"min": map[string]string{"cpu": strconv.Itoa(p.MinCPU / 1000), "mem": strconv.Itoa(p.MinMem/1024) + "Gi"},
		"max": map[string]string{"cpu": strconv.Itoa(p.MaxCPU / 1000), "mem": strconv.Itoa(p.MaxMem/1024) + "Gi"}})
	return string(b), err
}

func validateBoundsVM(vm map[string]any, p boundsPayload, live bool) error {
	if !owned(vm, p.ProjectID, p.EndpointID) || nested(vm, "metadata", "name") != p.WorkloadName ||
		vm["kind"] != "VirtualMachine" || vm["apiVersion"] != "vm.neon.tech/v1" {
		return errors.New("bounds VM ownership mismatch")
	}
	guest, ok := nested(vm, "spec", "guest").(map[string]any)
	if !ok || guest["memorySlotSize"] != "1Gi" {
		return errors.New("bounds VM guest capacity unavailable or unsupported")
	}
	minCPU, maxCPU := cpuMilli(nested(guest, "cpus", "min")), cpuMilli(nested(guest, "cpus", "max"))
	minMem := int(number(nested(guest, "memorySlots", "min"))) * 1024
	maxMem := int(number(nested(guest, "memorySlots", "max"))) * 1024
	if minCPU < 1000 || maxCPU < minCPU || minMem < 1024 || maxMem < minMem ||
		p.MinCPU < minCPU || p.MaxCPU > maxCPU || p.MinMem < minMem || p.MaxMem > maxMem {
		return errors.New("requested bounds exceed immutable VM capacity")
	}
	if live && (cpuMilli(nested(guest, "cpus", "use")) > p.MaxCPU ||
		int(number(nested(guest, "memorySlots", "use")))*1024 > p.MaxMem) {
		return errors.New("requested maximum is below current VM allocation")
	}
	return nil
}

func (k *kubeClient) resumeBoundsRoute(ctx context.Context, p boundsPayload) (map[string]any, map[string]any, map[string]any, error) {
	secret, err := k.request(ctx, http.MethodGet, k.path("secret", routesSecret), nil)
	if err != nil {
		return nil, nil, nil, err
	}
	if stringVal(nested(secret, "metadata", "resourceVersion")) == "" {
		return nil, nil, nil, errors.New("route registry resourceVersion missing")
	}
	raw, err := secretText(secret, "routes.json")
	if err != nil {
		return nil, nil, nil, err
	}
	var routes map[string]any
	if err := json.Unmarshal([]byte(raw), &routes); err != nil {
		return nil, nil, nil, errors.New("route registry is invalid")
	}
	route, ok := routes[selector(p.EndpointID)].(map[string]any)
	if !ok || route["project_id"] != p.ProjectID || route["kind"] != "neonvm" || route["workload"] != p.WorkloadName {
		return nil, nil, nil, errors.New("bounds Proxy route ownership mismatch")
	}
	template, ok := route["vm_template"].(map[string]any)
	if !ok {
		return nil, nil, nil, errors.New("bounds Proxy resume template missing")
	}
	if namespace := stringVal(nested(template, "metadata", "namespace")); namespace != "" && namespace != k.namespace {
		return nil, nil, nil, errors.New("bounds resume template namespace mismatch")
	}
	if err := validateBoundsVM(template, p, false); err != nil {
		return nil, nil, nil, err
	}
	return secret, routes, template, nil
}

// Persist the resume template first: any subsequent wake must inherit the
// desired soft bounds. A live patch failure leaves a retryable partial state;
// the worker commits endpoint metadata only after both resources converge.
// resourceVersion CAS and fresh reads preserve concurrent route/annotation
// updates. This is not a replacement for cross-instance connection fencing.
func (k *kubeClient) reconcileVMBounds(ctx context.Context, p boundsPayload) error {
	annotation, err := boundsAnnotation(p)
	if err != nil {
		return err
	}
	vm, err := k.request(ctx, http.MethodGet, k.path("vm", p.WorkloadName), nil)
	if err == nil {
		if err := validateBoundsVM(vm, p, true); err != nil {
			return err
		}
	} else if !kubeStatusIs(err, http.StatusNotFound) {
		return err
	}
	if err := k.persistResumeBounds(ctx, p, annotation); err != nil {
		return err
	}
	if err := k.patchLiveVMBounds(ctx, p, annotation); err != nil {
		return err
	}
	_, _, template, err := k.resumeBoundsRoute(ctx, p)
	if err != nil {
		return err
	}
	if nested(template, "metadata", "annotations", autoscalingBoundsAnnotation) != annotation {
		return errors.New("resume bounds changed before convergence was verified")
	}
	return nil
}

func (k *kubeClient) persistResumeBounds(ctx context.Context, p boundsPayload, annotation string) error {
	for attempt := 0; attempt < 6; attempt++ {
		secret, routes, template, err := k.resumeBoundsRoute(ctx, p)
		if err != nil {
			return err
		}
		if nested(template, "metadata", "annotations", autoscalingBoundsAnnotation) == annotation {
			return nil
		}
		meta := template["metadata"].(map[string]any)
		annotations, ok := meta["annotations"].(map[string]any)
		if !ok && meta["annotations"] != nil {
			return errors.New("resume template annotations are invalid")
		}
		if annotations == nil {
			annotations = map[string]any{}
			meta["annotations"] = annotations
		}
		annotations[autoscalingBoundsAnnotation] = annotation
		encoded, err := json.Marshal(routes)
		if err != nil {
			return err
		}
		data, ok := secret["data"].(map[string]any)
		if !ok {
			return errors.New("route registry Secret data missing")
		}
		data["routes.json"] = base64.StdEncoding.EncodeToString(encoded)
		_, err = k.request(ctx, http.MethodPut, k.path("secret", routesSecret), secret)
		if kubeStatusIs(err, http.StatusConflict) {
			continue
		}
		return err
	}
	return errors.New("resume bounds update conflict")
}

func (k *kubeClient) patchLiveVMBounds(ctx context.Context, p boundsPayload, annotation string) error {
	for attempt := 0; attempt < 6; attempt++ {
		vm, err := k.request(ctx, http.MethodGet, k.path("vm", p.WorkloadName), nil)
		if kubeStatusIs(err, http.StatusNotFound) {
			// Updating a suspended endpoint must not wake it just to change bounds.
			return nil
		}
		if err != nil {
			return err
		}
		if err := validateBoundsVM(vm, p, true); err != nil {
			return err
		}
		if nested(vm, "metadata", "deletionTimestamp") != nil ||
			nested(vm, "metadata", "annotations", autoscalingBoundsAnnotation) == annotation {
			return nil
		}
		uid := stringVal(nested(vm, "metadata", "uid"))
		rv := stringVal(nested(vm, "metadata", "resourceVersion"))
		if uid == "" || rv == "" {
			return errors.New("live bounds VM identity/version missing")
		}
		patch := map[string]any{"metadata": map[string]any{"uid": uid, "resourceVersion": rv,
			"annotations": map[string]string{autoscalingBoundsAnnotation: annotation}}}
		updated, err := k.request(ctx, http.MethodPatch, k.path("vm", p.WorkloadName), patch)
		if kubeStatusIs(err, http.StatusConflict) || kubeStatusIs(err, http.StatusNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if !owned(updated, p.ProjectID, p.EndpointID) || nested(updated, "metadata", "uid") != uid ||
			nested(updated, "metadata", "annotations", autoscalingBoundsAnnotation) != annotation {
			return errors.New("live bounds patch result mismatch")
		}
		return nil
	}
	return errors.New("live bounds update conflict")
}
