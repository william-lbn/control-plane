package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

func (s *Server) wake(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r, s.config.ProxyToken) {
		return
	}
	key, item, ok := s.selected(w, r)
	if !ok {
		return
	}
	select {
	case s.wakeSlots <- struct{}{}:
		defer func() { <-s.wakeSlots }()
	default:
		w.Header().Set("Retry-After", "1")
		reply(w, 503, map[string]any{"error": "adapter_busy"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.config.WakeTimeout)
	defer cancel()
	unlock, err := s.locks.acquire(ctx, key)
	if err != nil {
		s.unavailable(w, "wake_lock", err)
		return
	}
	defer unlock()
	cold, err := s.wakeWorkload(ctx, key, item)
	if err != nil {
		s.unavailable(w, "wake", err)
		return
	}
	info := "warm"
	if cold {
		info = "pool_miss"
		s.coldWakes.Add(1)
	}
	reply(w, 200, map[string]any{"address": item.Address, "aux": map[string]any{"endpoint_id": key, "project_id": item.ProjectID, "branch_id": item.BranchID, "compute_id": item.Workload, "cold_start_info": info}})
}

// Fresh route reads before mutations and response reject a closed/replaced route.
// They reduce stale work; a read-check/POST gap is not a distributed fence.
func (s *Server) sameRoute(ctx context.Context, key string, expected route) error {
	routes, err := s.routes(ctx)
	if err != nil {
		return err
	}
	current, ok := routes[key]
	if !ok || (current.State != "" && current.State != "active" && current.State != "ready" && current.State != "suspended") {
		return errors.New("route is no longer admitted")
	}
	if err = s.validateRoute(key, current); err != nil {
		return err
	}
	identity := func(item route) [32]byte {
		value, _ := json.Marshal([]any{item.ProjectID, item.BranchID, item.TenantID, item.Kind, item.Workload, item.Address, item.Template})
		return sha256.Sum256(value)
	}
	if identity(current) != identity(expected) {
		return errors.New("route identity changed")
	}
	return nil
}
func (s *Server) ownsVM(key string, item route, vm map[string]any) bool {
	uid, ok := nested(vm, "metadata", "uid").(string)
	if nested(vm, "metadata", "name") != item.Workload || !ok || uid == "" {
		return false
	}
	if len(item.Template) == 0 {
		return true
	} // Retained imported static VM: observe only.
	return nested(vm, "metadata", "labels", "neon-control/project-id") == item.ProjectID && nested(vm, "metadata", "labels", "neon-control/endpoint-id") == "ep_"+strings.TrimPrefix(key, "ep-")
}
func (s *Server) wakeWorkload(ctx context.Context, key string, item route) (bool, error) {
	if err := s.sameRoute(ctx, key, item); err != nil {
		return false, err
	}
	cold := false
	posted := false
	path := s.path("vm", item.Workload)
	if item.Kind == "deployment" {
		path = s.path("deployment", item.Workload)
	}
	if item.Kind == "deployment" {
		// The trusted route may reference a retained legacy Deployment. Preserve its
		// resourceVersion in /scale; uncertain PUT outcomes are not replayed.
		scale, err := s.kube.Request(ctx, http.MethodGet, path+"/scale", nil)
		if err != nil {
			return false, err
		}
		if replicas := nested(scale, "spec", "replicas"); replicas == float64(0) || replicas == json.Number("0") || replicas == 0 {
			if nested(scale, "metadata", "resourceVersion") == nil {
				return false, errors.New("scale version missing")
			}
			if err = s.sameRoute(ctx, key, item); err != nil {
				return false, err
			}
			spec, ok := scale["spec"].(map[string]any)
			if !ok {
				return false, errors.New("scale spec missing")
			}
			spec["replicas"] = 1
			if _, err = s.kube.Request(ctx, http.MethodPut, path+"/scale", scale); err != nil {
				return false, err
			}
			cold = true
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return cold, err
		}
		vm, err := s.kube.Request(ctx, http.MethodGet, path, nil)
		if isStatus(err, http.StatusNotFound) && len(item.Template) > 0 {
			if posted {
				return cold, errors.New("VM disappeared after wake admission")
			}
			if err = s.sameRoute(ctx, key, item); err != nil {
				return cold, err
			}
			// One POST only. 409 is observed by the next GET and exact ownership check;
			// a transport failure has an unknown outcome and is returned to the caller.
			posted = true
			_, err = s.kube.Request(ctx, http.MethodPost, s.path("vm", ""), item.Template)
			if err != nil && !isStatus(err, http.StatusConflict) {
				return cold, err
			}
			cold = true
		} else if err != nil {
			return cold, err
		} else {
			if item.Kind == "neonvm" && !s.ownsVM(key, item, vm) {
				return cold, errors.New("existing VM ownership mismatch")
			}
			if nested(vm, "metadata", "deletionTimestamp") == nil {
				ready := nested(vm, "status", "phase") == "Running"
				if item.Kind == "deployment" {
					value := nested(vm, "status", "readyReplicas")
					ready = value == float64(1) || value == json.Number("1") || value == 1
				}
				if ready {
					if err = s.sameRoute(ctx, key, item); err != nil {
						return cold, err
					}
					return cold, nil
				}
			}
		}
		if err = pause(ctx, s.config.PollInterval); err != nil {
			return cold, err
		}
	}
}
