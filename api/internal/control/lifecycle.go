package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type suspendPayload struct {
	ProjectID     string `json:"project_id"`
	EndpointID    string `json:"endpoint_id"`
	WorkloadName  string `json:"workload_name"`
	ExpectedVMUID string `json:"expected_vm_uid,omitempty"`
}

const idleGuardSQL = `SELECT
 (SELECT count(*) FROM pg_stat_activity WHERE pid<>pg_backend_pid()
  AND backend_type='client backend' AND usename<>'control_probe' AND client_addr IS NOT NULL
  AND NOT (client_addr <<= '127.0.0.0/8'::inet) AND NOT (client_addr <<= '::1/128'::inet)),
 (SELECT count(*) FROM pg_stat_replication),
 (SELECT count(*) FROM pg_stat_subscription WHERE pid IS NOT NULL),
 (SELECT count(*) FROM pg_stat_activity WHERE backend_type='autovacuum worker')`

type computeBusyError struct{}

func (computeBusyError) Error() string { return "compute has client sessions or background work" }

// Await normal connection closure, without terminating a session or excluding
// a service login from the SQL guard. The caller holds the exclusive internal
// probe gate; external admission fencing remains a separate production gate.
func awaitComputeIdle(ctx context.Context, window, interval time.Duration, read func(context.Context) (sqlResult, error)) error {
	guardCtx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := read(guardCtx)
		if err != nil {
			return fmt.Errorf("idle guard query: %w", err)
		}
		if len(result.Rows) != 1 || len(result.Rows[0]) != 4 {
			return errors.New("idle guard result invalid")
		}
		idle := true
		for _, count := range result.Rows[0] {
			if fmt.Sprint(count) != "0" {
				idle = false
			}
		}
		if idle {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-guardCtx.Done():
			timer.Stop()
			if err := ctx.Err(); err != nil {
				return err
			}
			return computeBusyError{}
		case <-timer.C:
		}
	}
}

func (s *server) suspendCompute(ctx context.Context, p suspendPayload) error {
	name := kubeName(p.EndpointID)
	if p.WorkloadName != name {
		return errors.New("suspend workload identity mismatch")
	}
	release, err := s.acquireProbeGate(ctx, p.EndpointID, false)
	if err != nil {
		return err
	}
	defer release()
	secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", routesSecret), nil)
	if err != nil {
		return err
	}
	raw, err := secretText(secret, "routes.json")
	if err != nil {
		return err
	}
	var routes map[string]map[string]any
	if err := json.Unmarshal([]byte(raw), &routes); err != nil {
		return err
	}
	route := routes[selector(p.EndpointID)]
	if route == nil || route["project_id"] != p.ProjectID || route["kind"] != "neonvm" || route["workload"] != name {
		return errors.New("suspend Proxy route ownership mismatch")
	}
	template, ok := route["vm_template"].(map[string]any)
	if !ok || nested(template, "metadata", "name") != name ||
		nested(template, "metadata", "labels", "neon-control/endpoint-id") != p.EndpointID {
		return errors.New("suspend Proxy recovery template missing or unowned")
	}
	vmPath := s.kube.path("vm", name)
	vm, err := s.kube.request(ctx, http.MethodGet, vmPath, nil)
	if err != nil {
		return err
	}
	if !owned(vm, p.ProjectID, p.EndpointID) || nested(vm, "status", "phase") != "Running" {
		return errors.New("suspend VM not running or unowned")
	}
	uid := stringVal(nested(vm, "metadata", "uid"))
	if uid == "" {
		return errors.New("suspend VM UID missing")
	}
	// Automatic idle evidence belongs to the VM observed by the controller.
	// A queued operation must not apply that evidence to a later cold wake.
	if p.ExpectedVMUID != "" && p.ExpectedVMUID != uid {
		return errors.New("automatic suspend VM generation changed")
	}
	config, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", name+"-config"), nil)
	if err != nil || !owned(config, p.ProjectID, p.EndpointID) {
		return errors.New("suspend probe Secret unavailable or unowned")
	}
	password, err := secretText(config, "probePassword")
	if err != nil {
		return err
	}
	if err = awaitComputeIdle(ctx, 10*time.Second, time.Second, func(guardCtx context.Context) (sqlResult, error) {
		return runSQL(guardCtx, s.proxyHost, s.proxyPort, "control_probe", password, "postgres", selector(p.EndpointID), idleGuardSQL)
	}); err != nil {
		return err
	}
	// Verify the exact VM still exists immediately before deletion. Kubernetes
	// UID preconditions prevent a stale controller from deleting a newer wake.
	vm, err = s.kube.request(ctx, http.MethodGet, vmPath, nil)
	if err != nil {
		return err
	}
	if !owned(vm, p.ProjectID, p.EndpointID) || nested(vm, "metadata", "uid") != uid || nested(vm, "status", "phase") != "Running" {
		return errors.New("suspend VM changed after idle guard")
	}
	_, err = s.kube.request(ctx, http.MethodDelete, vmPath, map[string]any{"apiVersion": "v1", "kind": "DeleteOptions",
		"preconditions": map[string]string{"uid": uid}})
	if err != nil {
		return err
	}
	return s.kube.waitVMGenerationDeletion(ctx, p, uid, 120*time.Second, 2*time.Second)
}

// A Proxy cold wake can replace the named VM between observations. Seeing an
// owned successor UID proves the deleted generation is gone, even when the
// brief 404 was missed. Never delete or wait for the successor to disappear.
func (k *kubeClient) waitVMGenerationDeletion(ctx context.Context, p suspendPayload, uid string, window, interval time.Duration) error {
	vmPath := k.path("vm", p.WorkloadName)
	for deadline := time.Now().Add(window); time.Now().Before(deadline); {
		vm, err := k.request(ctx, http.MethodGet, vmPath, nil)
		var ke kubeError
		if errors.As(err, &ke) && ke.Status == http.StatusNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		if !owned(vm, p.ProjectID, p.EndpointID) {
			return errors.New("VM deletion observation ownership mismatch")
		}
		observed := stringVal(nested(vm, "metadata", "uid"))
		if observed == "" {
			return errors.New("VM deletion observation UID missing")
		}
		if observed != uid {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return errors.New("VM deletion was not observed before timeout")
}

func (s *server) queueSuspend(ctx context.Context, p suspendPayload, actor, request string) (string, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE resource_type='endpoint'
		AND resource_id=$1 AND state IN ('queued','running','retry_wait'))`, p.EndpointID).Scan(&exists); err != nil {
		return "", err
	}
	if exists {
		return "", errors.New("endpoint operation already active")
	}
	opID := newID("op_")
	data, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload)
		VALUES($1,$2,'endpoint',$3,'suspend_endpoint','queued',$4,$5,$6)`, opID, p.ProjectID, p.EndpointID, actor, request, data)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,0,'verify_idle_and_delete_neonvm','queued')`, opID)
	if err != nil {
		return "", err
	}
	return opID, tx.Commit(ctx)
}

func (s *server) suspendEndpoint(w http.ResponseWriter, r *http.Request) {
	item, ok := s.requireEndpoint(w, r)
	if !ok {
		return
	}
	if item["workload_kind"] != "neonvm" || item["state"] != "active" {
		fail(w, r, 422, "unsupported_endpoint", "Only a ready managed NeonVM can suspend")
		return
	}
	p := suspendPayload{ProjectID: r.PathValue("project"), EndpointID: r.PathValue("endpoint"), WorkloadName: stringVal(item["workload_name"])}
	opID, err := s.queueSuspend(r.Context(), p, userFrom(r).ID, requestID(r))
	if err != nil {
		fail(w, r, 409, "operation_conflict", "Could not queue suspend operation")
		return
	}
	s.audit(r.Context(), userFrom(r).ID, "suspend_endpoint", "endpoint", p.EndpointID, "accepted", requestID(r))
	op, _ := s.operationRecord(r.Context(), opID, p.ProjectID)
	jsonResponse(w, 202, map[string]any{"resource": item, "operation": op})
}

func (s *server) endpointLifecycle(w http.ResponseWriter, r *http.Request) {
	item, ok := s.requireEndpoint(w, r)
	if !ok {
		return
	}
	if item["workload_kind"] != "neonvm" || item["state"] != "active" {
		fail(w, r, 422, "unsupported_endpoint", "Only a ready managed NeonVM supports lifecycle settings")
		return
	}
	var body struct {
		ScaleToZero        bool `json:"scale_to_zero"`
		IdleTimeoutSeconds int  `json:"idle_timeout_seconds"`
	}
	if err := readJSON(r, &body); err != nil || body.IdleTimeoutSeconds < 60 || body.IdleTimeoutSeconds > 3600 {
		fail(w, r, 422, "invalid_lifecycle", "idle_timeout_seconds must be 60-3600")
		return
	}
	if r.Header.Get("If-Match") != fmt.Sprintf("\"%v\"", item["version"]) {
		fail(w, r, 412, "version_mismatch", "Reload endpoint before editing")
		return
	}
	tag, err := s.db.Exec(r.Context(), `UPDATE endpoints SET scale_to_zero=$3,idle_timeout_seconds=$4,
		version=version+1,updated_at=now() WHERE id=$1 AND project_id=$2 AND version=$5`,
		r.PathValue("endpoint"), r.PathValue("project"), body.ScaleToZero, body.IdleTimeoutSeconds, item["version"])
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not update lifecycle")
		return
	}
	if tag.RowsAffected() != 1 {
		fail(w, r, 412, "version_mismatch", "Reload endpoint before editing")
		return
	}
	s.audit(r.Context(), userFrom(r).ID, "update_lifecycle", "endpoint", r.PathValue("endpoint"), "succeeded", requestID(r))
	updated, _ := s.one(r.Context(), "SELECT * FROM endpoints WHERE id=$1", r.PathValue("endpoint"))
	jsonResponse(w, 200, s.decorateEndpoint(r.Context(), updated))
}

func (s *server) runIdleController(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			items, err := s.many(ctx, `SELECT id,project_id,workload_name,idle_timeout_seconds FROM endpoints
			WHERE scale_to_zero AND state='active' AND workload_kind='neonvm' AND deleted_at IS NULL`)
			if err != nil {
				s.logger.Error("idle candidates", "error", err)
				continue
			}
			for _, item := range items {
				endpointID := stringVal(item["id"])
				runtime := s.kube.runtime(ctx, "neonvm", stringVal(item["workload_name"]))
				if runtime["observed_state"] != "active" {
					continue
				}
				seconds, ok := item["idle_timeout_seconds"].(int32)
				if !ok {
					continue
				}
				born, parseErr := time.Parse(time.RFC3339Nano, stringVal(runtime["workload_created_at"]))
				if parseErr != nil || stringVal(runtime["workload_uid"]) == "" {
					continue
				}
				samples, readErr := s.readIdleSamples(ctx, endpointID, born, int(seconds))
				if readErr != nil || !idleWindowEligible(samples, time.Now(), born, time.Duration(seconds)*time.Second) {
					continue
				}
				p := suspendPayload{ProjectID: stringVal(item["project_id"]), EndpointID: endpointID, WorkloadName: stringVal(item["workload_name"]), ExpectedVMUID: stringVal(runtime["workload_uid"])}
				if _, err = s.queueSuspend(ctx, p, "system:idle-controller", newID("req_")); err != nil {
					s.logger.Warn("idle suspend enqueue", "endpoint_id", endpointID, "error_type", fmt.Sprintf("%T", err))
				}
			}
		}
	}
}
