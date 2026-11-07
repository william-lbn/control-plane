package control

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functionedge"
	"github.com/william-lbn/control-plane/api/internal/functions"
)

func (s *server) beginFunctionEdgeEvent(ctx context.Context, r *http.Request, runtime publishedFunctionRuntime, bytes int, method string, deadline time.Time) (functionEvent, error) {
	event := functionEvent{ID: newID("fre_"), Started: time.Now()}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return event, err
	}
	defer tx.Rollback(ctx)
	p := runtime.target
	i := runtime.instance.Scope.InstanceID
	_, err = tx.Exec(ctx, `INSERT INTO function_request_events(id,request_id,function_id,project_id,branch_id,deployment_id,instance_id,channel,method,outcome,started_at,deadline_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,'public_http',$8,'in_progress',$9,$10)`, event.ID, requestID(r), p.FunctionID, p.ProjectID, p.BranchID, p.DeploymentID, i, method, event.Started, deadline)
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO function_edge_invocations(event_id,function_id,instance_id,deployment_id,boot_id,definition_version,edge_identity,request_bytes)
 VALUES($1,$2,$3,$4,$5,$6,'neon-function-edge',$7)`, event.ID, p.FunctionID, i, p.DeploymentID, runtime.bootID, runtime.version, bytes)
	}
	if err != nil {
		return event, err
	}
	return event, tx.Commit(ctx)
}

func (s *server) completeFunctionEdgeEvent(event functionEvent, v functionedge.Completion) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE function_edge_invocations SET dispatched=$2,response_bytes=$3,duration_ms=$4,outcome=$5,stream_request_bytes=$6,completed_at=clock_timestamp()
 WHERE event_id=$1 AND completed_at IS NULL`, event.ID, v.Dispatched, v.ResponseBytes, v.DurationMS, v.Outcome, v.StreamRequestBytes)
	if err != nil || result.RowsAffected() != 1 {
		return false
	}
	var status any
	if v.Status >= 100 && v.Status <= 599 {
		status = v.Status
	}
	outcome := v.Outcome
	if outcome == "not_dispatched" {
		outcome = "outcome_unknown"
	}
	result, err = tx.Exec(ctx, `UPDATE function_request_events SET outcome=$2,application_status=$3,response_bytes=$4,duration_ms=$5,finished_at=clock_timestamp()
 WHERE id=$1 AND outcome='in_progress'`, event.ID, outcome, status, v.ResponseBytes, v.DurationMS)
	if err != nil || result.RowsAffected() != 1 {
		return false
	}
	return tx.Commit(ctx) == nil
}

func (s *server) sampleFunctionResources(ctx context.Context, runtime publishedFunctionRuntime, client *functions.InstanceClient) {
	v, err := client.Resources(ctx)
	if err != nil || v.BootID != runtime.bootID || v.Scope != runtime.instance.Scope {
		return
	}
	_ = s.recordFunctionResourceSample(ctx, runtime.target.FunctionID, v)
}

func (s *server) recordFunctionResourceSample(ctx context.Context, functionID string, v functions.ResourceMeter) error {
	if !functionDefinitionID.MatchString(functionID) || v.Scope.Validate() != nil || !functionBootID.MatchString(v.BootID) ||
		v.Source != "supervisor_cgroup_v2" || v.CPUUsec < 0 || v.MemoryBytes < 0 || v.MemoryPeakBytes < v.MemoryBytes || v.ObservedAt.IsZero() {
		return errors.New("function resource scope or counters rejected")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Serialize the original boot's counter ordering, including concurrent edge
	// completions and the periodic collector. Counter regression is not usage.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,794210173))`, v.Scope.InstanceID+":"+v.BootID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO function_resource_samples(function_id,instance_id,deployment_id,boot_id,cpu_usec,memory_bytes,memory_peak_bytes,observed_at,source)
 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9 WHERE $8::timestamptz BETWEEN clock_timestamp()-interval '30 seconds' AND clock_timestamp()+interval '2 seconds'
 AND EXISTS(SELECT 1 FROM function_instances i JOIN function_definitions f ON f.id=i.function_id WHERE i.id=$2 AND i.function_id=$1 AND i.deployment_id=$3 AND i.boot_id=$4 AND i.generation=$10 AND i.project_id=$11 AND i.branch_id=$12 AND f.slug=$13)
 AND NOT EXISTS(SELECT 1 FROM function_resource_samples WHERE instance_id=$2 AND boot_id=$4 AND (observed_at>$8 OR cpu_usec>$5))
 ON CONFLICT(instance_id,boot_id,observed_at) DO NOTHING`, functionID, v.Scope.InstanceID, v.Scope.DeploymentID, v.BootID, v.CPUUsec, v.MemoryBytes, v.MemoryPeakBytes, v.ObservedAt, v.Source, v.Scope.Generation, v.Scope.ProjectID, v.Scope.BranchID, v.Scope.Slug)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Small fixed local-preview fleet. Sample privileged cumulative counters even
// when no public request is active, including customer background work. No
// customer HTTP call is made and failed observations remain missing/stale.
func (s *server) runFunctionResourceMeter(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	cursor := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		rows, err := s.functionResourceMeterCandidates(ctx, cursor)
		if err != nil {
			continue
		}
		if len(rows) == 0 {
			cursor = ""
			continue
		}
		for _, row := range rows {
			if ctx.Err() != nil {
				return
			}
			// Advance past failed observations too; an unavailable early Function
			// must not permanently starve later definitions.
			cursor = stringVal(row["id"])
			callCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
			runtime, err := s.loadPublishedFunction(callCtx, stringVal(row["project_id"]), stringVal(row["branch_id"]), stringVal(row["slug"]))
			if err == nil {
				client, _, err := s.observePublishedFunction(callCtx, runtime)
				if err == nil {
					s.sampleFunctionResources(callCtx, runtime, client)
					client.Close()
				}
			}
			cancel()
		}
	}
}

func (s *server) functionResourceMeterCandidates(ctx context.Context, after string) ([]record, error) {
	return s.many(ctx, `SELECT id,project_id,branch_id,slug FROM function_definitions WHERE state='active' AND id>$1 ORDER BY id LIMIT 8`, after)
}

func (s *server) functionUsage(w http.ResponseWriter, r *http.Request) {
	if !s.functionReadBranch(w, r) {
		return
	}
	var id, state string
	err := s.db.QueryRow(r.Context(), `SELECT id,state FROM function_definitions WHERE project_id=$1 AND branch_id=$2 AND slug=$3 AND state<>'deleted'`, r.PathValue("project"), r.PathValue("branch"), r.PathValue("slug")).Scan(&id, &state)
	if isNoRows(err) {
		fail(w, r, 404, "not_found", "Function not found")
		return
	}
	if err != nil {
		fail(w, r, 503, "function_usage_unavailable", "Function metadata unavailable")
		return
	}
	invocations, err := s.many(r.Context(), `SELECT channel,count(*)::bigint AS admitted_invocations,
 count(*) FILTER(WHERE outcome='completed')::bigint AS completed_invocations,
 count(*) FILTER(WHERE outcome='in_progress' AND deadline_at>=clock_timestamp())::bigint AS in_progress_invocations,
 count(*) FILTER(WHERE outcome='outcome_unknown' OR outcome='in_progress' AND deadline_at<clock_timestamp())::bigint AS unknown_invocations,
 COALESCE(sum(response_bytes),0)::bigint AS observed_response_bytes,COALESCE(sum(duration_ms),0)::bigint AS observed_duration_ms
 FROM function_request_events WHERE function_id=$1 GROUP BY channel ORDER BY channel`, id)
	if err != nil {
		fail(w, r, 503, "function_usage_unavailable", "Invocation usage unavailable")
		return
	}
	edge, err := s.one(r.Context(), `SELECT count(*)::bigint AS admitted_invocations,count(*) FILTER(WHERE dispatched)::bigint AS dispatch_attempts,
 count(*) FILTER(WHERE dispatched IS NULL)::bigint AS unconfirmed_dispatches,count(*) FILTER(WHERE completed_at IS NOT NULL)::bigint AS completion_receipts,
 COALESCE(sum(request_bytes),0)::bigint AS admitted_request_bytes,COALESCE(sum(response_bytes),0)::bigint AS observed_response_bytes,
 COALESCE(sum(stream_request_bytes),0)::bigint AS observed_websocket_request_bytes
 FROM function_edge_invocations WHERE function_id=$1`, id)
	if err != nil {
		fail(w, r, 503, "function_usage_unavailable", "Edge usage unavailable")
		return
	}
	resources, err := s.many(r.Context(), `SELECT instance_id,deployment_id,boot_id,max(cpu_usec)::bigint AS observed_cpu_usec,
 (array_agg(memory_bytes ORDER BY observed_at DESC))[1] AS observed_memory_current_bytes,
 max(memory_peak_bytes)::bigint AS observed_memory_peak_bytes,count(*)::bigint AS sample_count,min(observed_at) AS first_observed_at,max(observed_at) AS last_observed_at,
 max(received_at)>=clock_timestamp()-interval '45 seconds' AS fresh
 FROM function_resource_samples WHERE function_id=$1 GROUP BY instance_id,deployment_id,boot_id ORDER BY min(observed_at)`, id)
	if err != nil {
		fail(w, r, 503, "function_usage_unavailable", "Resource observations unavailable")
		return
	}
	resourceStatus := "unavailable"
	if len(resources) > 0 {
		resourceStatus = "historical_only"
	}
	var currentFresh bool
	err = s.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM function_resource_samples a JOIN function_instances i ON i.id=a.instance_id AND i.boot_id=a.boot_id
 JOIN function_definitions f ON f.id=a.function_id AND f.active_deployment_id=a.deployment_id
 WHERE f.id=$1 AND f.state='active' AND i.state='ready' AND a.received_at>=clock_timestamp()-interval '45 seconds')`, id).Scan(&currentFresh)
	if err != nil {
		fail(w, r, 503, "function_usage_unavailable", "Current resource observations unavailable")
		return
	}
	if currentFresh {
		resourceStatus = "observed"
	}
	jsonResponse(w, 200, record{"function_id": id, "state": state, "period": "retained_lifetime", "invocations": invocations, "edge": edge,
		"resource_observations": resources, "resource_status": resourceStatus,
		"resource_source": "authenticated_supervisor_cgroup_v2", "cpu_basis": "cumulative_per_original_boot_not_per_request",
		"memory_basis": "observed_current_and_peak_not_integrated_gb_hours", "billing": "usage_observation_no_automatic_credit_debit"})
}
