package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functions"
	"github.com/william-lbn/control-plane/api/internal/triggers"
)

// Snapshots contain routing metadata only. Customer bodies, credentials, URLs,
// headers and environment are never persisted in the occurrence queue.
type functionTriggerSnapshot struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
	Path string `json:"path"`
}
type functionTriggerRun struct {
	ID, Trigger, Project, Branch, Function, Actor, Owner string
	Generation                                           int64
	Scheduled                                            time.Time
	Snapshot                                             functionTriggerSnapshot
	Attempts                                             int
}

func (s *server) runFunctionScheduleController(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := s.enqueueFunctionSchedules(callCtx)
			cancel()
			if err != nil && s.logger != nil {
				s.logger.Warn("function schedule admission unavailable")
			}
		}
	}
}

// PostgreSQL time and row locks are authoritative. At most one occurrence per
// schedule is caught up per tick. Misses older than two minutes are journaled as
// skipped and the plan advances to its next future UTC minute, avoiding storms.
func (s *server) enqueueFunctionSchedules(ctx context.Context) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT t.id,t.project_id,t.branch_id,t.function_id,t.actor_id,t.generation,t.next_run_at,t.name,f.slug,t.function_path,t.cron
 FROM function_triggers t JOIN function_definitions f ON f.id=t.function_id
 JOIN branches b ON b.id=t.branch_id JOIN projects p ON p.id=t.project_id JOIN organizations o ON o.id=t.org_id
 WHERE t.state='ready' AND t.enabled AND t.next_run_at<=$1 AND f.state NOT IN ('deleting','deleted')
 AND b.state='ready' AND b.deleted_at IS NULL AND p.state='ready' AND p.deleted_at IS NULL AND o.state='active'
 ORDER BY t.next_run_at,t.id LIMIT 16 FOR UPDATE OF t SKIP LOCKED`, now)
	if err != nil {
		return err
	}
	type due struct {
		run  functionTriggerRun
		cron string
	}
	items := []due{}
	for rows.Next() {
		var item due
		err = rows.Scan(&item.run.Trigger, &item.run.Project, &item.run.Branch, &item.run.Function, &item.run.Actor, &item.run.Generation, &item.run.Scheduled, &item.run.Snapshot.Name, &item.run.Snapshot.Slug, &item.run.Snapshot.Path, &item.cron)
		if err != nil {
			rows.Close()
			return err
		}
		item.run.Snapshot.ID = item.run.Trigger
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range items {
		run := item.run
		id, e := triggers.ScheduleInvocationID(run.Branch, run.Trigger, run.Scheduled)
		if e != nil {
			return e
		}
		cron, e := triggers.ParseCron(item.cron)
		if e != nil {
			return e
		}
		next, e := cron.Next(now)
		if e != nil {
			return e
		}
		snapshot, e := json.Marshal(run.Snapshot)
		if e != nil {
			return e
		}
		state := "queued"
		var finished any
		var code any
		if now.Sub(run.Scheduled) > 2*time.Minute {
			state = "skipped"
			finished = now
			code = "schedule_misfire"
		}
		_, err = tx.Exec(ctx, `INSERT INTO function_trigger_invocations(id,trigger_id,project_id,branch_id,function_id,actor_id,trigger_generation,scheduled_at,snapshot,state,finished_at,error_code)
  VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(trigger_id,branch_id,scheduled_at) DO NOTHING`, id, run.Trigger, run.Project, run.Branch, run.Function, run.Actor, run.Generation, run.Scheduled, snapshot, state, finished, code)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE function_triggers SET next_run_at=$3 WHERE branch_id=$1 AND id=$2`, run.Branch, run.Trigger, next); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *server) runFunctionTriggerDispatcher(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run, err := s.claimFunctionTrigger(ctx)
			if err != nil || run == nil {
				continue
			}
			// One live dispatch per controller, independent of the enqueue loop. Lease
			// duration exceeds this deadline; an expired lease never grants replay.
			callCtx, cancel := context.WithTimeout(ctx, 240*time.Second)
			s.dispatchFunctionTrigger(callCtx, *run)
			cancel()
		}
	}
}

func (s *server) claimFunctionTrigger(ctx context.Context) (*functionTriggerRun, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	// A crash can occur on either side of network dispatch. Never infer that an
	// expired owner's call did not execute. Its durable record remains unknown.
	_, err = tx.Exec(ctx, `UPDATE function_trigger_invocations SET state='outcome_unknown',error_code='dispatch_lease_expired',finished_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL WHERE state='dispatching' AND lease_expires_at<clock_timestamp()`)
	if err != nil {
		return nil, err
	}
	run := &functionTriggerRun{Owner: newID("trigger_worker_")}
	var raw []byte
	err = tx.QueryRow(ctx, `WITH candidate AS (SELECT id FROM function_trigger_invocations WHERE state='queued' AND next_attempt_at<=clock_timestamp() AND attempts<3 ORDER BY next_attempt_at,admitted_at,id LIMIT 1 FOR UPDATE SKIP LOCKED)
 UPDATE function_trigger_invocations i SET state='dispatching',attempts=attempts+1,lease_owner=$1,lease_expires_at=clock_timestamp()+interval '300 seconds',started_at=COALESCE(started_at,clock_timestamp())
 FROM candidate c WHERE i.id=c.id RETURNING i.id,i.trigger_id,i.project_id,i.branch_id,i.function_id,i.actor_id,i.trigger_generation,i.scheduled_at,i.snapshot,i.attempts`, run.Owner).Scan(&run.ID, &run.Trigger, &run.Project, &run.Branch, &run.Function, &run.Actor, &run.Generation, &run.Scheduled, &raw, &run.Attempts)
	if isNoRows(err) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &run.Snapshot); err != nil {
		return nil, err
	}
	if !functionTriggerID.MatchString(run.Snapshot.ID) || run.Trigger != run.Snapshot.ID || !triggers.ValidName(run.Snapshot.Name) || !functions.ValidSlug(run.Snapshot.Slug) || !triggers.ValidPath(run.Snapshot.Path) {
		return nil, errors.New("invalid durable trigger snapshot")
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return run, nil
}

func (s *server) finishFunctionTrigger(run functionTriggerRun, state, code string, status int, responseBytes int64, retry bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if retry && run.Attempts < 3 {
		s.db.Exec(ctx, `UPDATE function_trigger_invocations SET state='queued',next_attempt_at=clock_timestamp()+interval '15 seconds',error_code=$3,lease_owner=NULL,lease_expires_at=NULL WHERE id=$1 AND state='dispatching' AND lease_owner=$2 AND lease_expires_at>clock_timestamp()`, run.ID, run.Owner, code)
		return
	}
	var applicationStatus any
	if status >= 100 && status <= 599 {
		applicationStatus = status
	}
	s.db.Exec(ctx, `UPDATE function_trigger_invocations SET state=$3,error_code=NULLIF($4,''),application_status=$5,response_bytes=$6,finished_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL WHERE id=$1 AND state='dispatching' AND lease_owner=$2 AND lease_expires_at>clock_timestamp()`, run.ID, run.Owner, state, code, applicationStatus, responseBytes)
}

func (s *server) dispatchFunctionTrigger(ctx context.Context, run functionTriggerRun) {
	fail := func(state, code string, retry bool) { s.finishFunctionTrigger(run, state, code, 0, 0, retry) }
	var enabledActor bool
	if s.db.QueryRow(ctx, `SELECT NOT disabled FROM users WHERE id=$1`, run.Actor).Scan(&enabledActor) != nil {
		fail("failed", "actor_unavailable", true)
		return
	}
	access, err := s.projectAccess(ctx, run.Project, run.Actor)
	if err != nil || !enabledActor || access.Level < 2 {
		fail("skipped", "actor_authorization_revoked", false)
		return
	}
	identity, err := s.functionHTTPIdentity(ctx, run.Branch, run.Snapshot.Slug)
	if err != nil || identity.ID != run.Function {
		fail("skipped", "target_authorization_unavailable", false)
		return
	}
	// Prevent waking a different function after a future slug replacement.
	identity, err = s.awaitFunctionIdentity(ctx, run.Branch, run.Snapshot.Slug, run.Function)
	if err != nil {
		fail("failed", "target_not_ready", true)
		return
	}
	if identity.ID != run.Function {
		fail("skipped", "target_identity_changed", false)
		return
	}
	release, ok := s.functionAdmissionLock(ctx, run.Function, false)
	if !ok {
		fail("failed", "target_admission_busy", true)
		return
	}
	defer release()
	identity, err = s.functionHTTPIdentity(ctx, run.Branch, run.Snapshot.Slug)
	if err != nil || identity.ID != run.Function || identity.State != "active" {
		fail("skipped", "target_admission_changed", false)
		return
	}
	runtime, err := s.loadPublishedFunction(ctx, run.Project, run.Branch, run.Snapshot.Slug)
	if err != nil || runtime.target.FunctionID != run.Function {
		fail("failed", "target_runtime_unavailable", true)
		return
	}
	client, status, err := s.observePublishedFunction(ctx, runtime)
	if err != nil {
		fail("failed", "target_observation_unavailable", true)
		return
	}
	defer client.Close()
	caps, err := client.Capabilities(ctx)
	if err != nil || caps.BootID != runtime.bootID || !caps.TriggerInvocations {
		fail("failed", "trigger_runtime_upgrade_required", false)
		return
	}
	if !status.NodeReady || status.Draining {
		fail("failed", "target_runtime_not_accepting", true)
		return
	}
	access, err = s.projectAccess(ctx, run.Project, run.Actor)
	if err != nil || access.Level < 2 || s.db.QueryRow(ctx, `SELECT NOT disabled FROM users WHERE id=$1`, run.Actor).Scan(&enabledActor) != nil || !enabledActor {
		fail("skipped", "actor_authorization_revoked", false)
		return
	}
	body, _ := json.Marshal(record{"version": 1, "invocation_id": run.ID, "trigger": record{"type": "schedule", "id": run.Trigger, "name": run.Snapshot.Name}, "data": record{"scheduled_at": run.Scheduled.UTC()}})
	invocation := functions.Invocation{BootID: runtime.bootID, Generation: runtime.target.Generation, Method: http.MethodPost, URL: functionPublicOrigin() + "/functions/v1/" + run.Branch + "/" + run.Snapshot.Slug + run.Snapshot.Path, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body, TriggerInvocationID: run.ID}
	if !functions.ValidateInvocation(runtime.instance.Scope, invocation) {
		fail("failed", "trigger_envelope_rejected", false)
		return
	}
	request, _ := http.NewRequestWithContext(context.WithValue(ctx, requestIDKey, run.ID), http.MethodPost, "http://schedule.internal", nil)
	event, err := s.beginFunctionEvent(ctx, request, runtime.target, runtime.instance.Scope.InstanceID, "schedule", http.MethodPost)
	if err != nil {
		fail("failed", "request_journal_unavailable", true)
		return
	}
	tag, err := s.db.Exec(ctx, `UPDATE function_trigger_invocations SET request_event_id=$3 WHERE id=$1 AND state='dispatching' AND lease_owner=$2 AND lease_expires_at>clock_timestamp() AND request_event_id IS NULL`, run.ID, run.Owner, event.ID)
	if err != nil || tag.RowsAffected() != 1 {
		s.finishFunctionEvent(event, 0, 0, "outcome_unknown")
		fail("outcome_unknown", "dispatch_admission_unconfirmed", false)
		return
	}
	response, err := client.Invoke(ctx, invocation)
	if err != nil {
		s.finishFunctionEvent(event, 0, 0, "outcome_unknown")
		fail("outcome_unknown", "dispatch_outcome_unknown", false)
		return
	}
	defer response.Body.Close()
	n, err := io.Copy(io.Discard, io.LimitReader(response.Body, functions.MaxInvokeBytes+1))
	if err != nil || n > functions.MaxInvokeBytes {
		s.finishFunctionEvent(event, response.StatusCode, n, "response_incomplete")
		s.finishFunctionTrigger(run, "outcome_unknown", "response_incomplete", response.StatusCode, n, false)
		return
	}
	s.finishFunctionEvent(event, response.StatusCode, n, "completed")
	state, code := "succeeded", ""
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		state, code = "failed", "application_response_failed"
	}
	s.finishFunctionTrigger(run, state, code, response.StatusCode, n, false)
}
