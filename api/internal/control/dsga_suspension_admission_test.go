package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neondatabase/autoscaling/pkg/dsga/runtimeselection"
)

func dsgaSuspensionAdmissionFixture(t *testing.T) (context.Context, *dsgaCreationBinding, dsgaInitializationResponse) {
	t.Helper()
	ctx, b, _, _, _, _, _, fact := initializationBindingFixture(t, "create_project")
	vm := fact.Observed.Instance.VMUID
	if _, err := b.supplyFact(ctx, "runtime_selection", dsgaSupplyObject{runtimeselection.Name(vm), "selection-original", strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	for _, setup := range []struct{ query, id string }{
		{"UPDATE operations SET state='succeeded',finished_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL WHERE id=$1", b.operationID},
		{"UPDATE projects SET state='ready' WHERE id=$1", b.intent.ProjectID},
		{"UPDATE branches SET state='ready' WHERE id=$1", b.intent.BranchID},
		{"UPDATE endpoints SET state='active' WHERE id=$1", b.intent.EndpointID},
	} {
		if _, err := b.server.db.Exec(ctx, setup.query, setup.id); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, b, fact
}

func TestDSGAAutomaticSuspensionBindsSamplesAndCurrentPolicy(t *testing.T) {
	ctx, b, fact := dsgaSuspensionAdmissionFixture(t)
	s := b.server
	project, endpoint := b.intent.ProjectID, b.intent.EndpointID
	vm := fact.Observed.Instance.VMUID
	t.Setenv("NEON_V2_CREATE_ENABLED", "true")
	t.Setenv("NEON_DSGA_AUTO_SUSPENSION_ENABLED", "true")
	eligible := dsgaIdleCandidate{vmUID: vm, window: 60}
	queue := func(version int64, candidate dsgaIdleCandidate) (string, error) {
		return s.queueDSGASuspension(ctx, project, endpoint, version, dsgaIdleActor, "idle-test", "", "", &candidate)
	}
	reject := func(version int64, candidate dsgaIdleCandidate, reason error) {
		t.Helper()
		if id, err := queue(version, candidate); id != "" || !errors.Is(err, reason) {
			t.Fatalf("unexpected idle admission: %s %v", id, err)
		}
	}
	var now time.Time
	if err := s.db.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	setPolicy := func(enabled bool, updated time.Time) {
		t.Helper()
		if _, err := s.db.Exec(ctx, `UPDATE endpoints SET scale_to_zero=$2,idle_timeout_seconds=60,updated_at=$3 WHERE id=$1`,
			endpoint, enabled, updated); err != nil {
			t.Fatal(err)
		}
	}
	put := func(at, boot time.Time, uid string, count int, sessions int64) {
		t.Helper()
		if _, err := s.db.Exec(ctx, `INSERT INTO metric_samples(endpoint_id,sampled_at,observed_state,connections,dsga_vm_uid,dsga_pg_started_at,dsga_pg_sessions)
 VALUES($1,$2,'active',$3,$4,$5,$6)`, endpoint, at, count, uid, boot, sessions); err != nil {
			t.Fatal(err)
		}
	}
	boot := now.Add(-time.Hour)
	put(now.Add(-95*time.Second), boot, vm, 0, 100)
	put(now.Add(-65*time.Second), boot, vm, 0, 101)
	put(now.Add(-35*time.Second), boot, vm, 0, 104)
	put(now.Add(-5*time.Second), boot, vm, 0, 105)
	setPolicy(false, now.Add(-2*time.Minute))
	reject(1, eligible, errDSGAIdleSuspensionIneligible)
	setPolicy(true, now.Add(-75*time.Second))
	reject(1, eligible, errDSGAIdleSuspensionIneligible)
	setPolicy(true, now.Add(-2*time.Minute))
	reject(1, eligible, errDSGAIdleSuspensionIneligible) // a short connection between probes
	reject(9, eligible, errDSGASuspensionVersion)
	reject(1, dsgaIdleCandidate{vmUID: "previous-vm", window: 60}, errDSGAIdleSuspensionIneligible)
	if _, err := s.db.Exec(ctx, "UPDATE metric_samples SET dsga_pg_sessions=dsga_pg_sessions-2 WHERE sampled_at>$1 AND endpoint_id=$2", now.Add(-40*time.Second), endpoint); err != nil {
		t.Fatal(err)
	}
	put(now.Add(-time.Second), boot.Add(20*time.Minute), vm, 0, 5)
	reject(1, eligible, errDSGAIdleSuspensionIneligible)
	if _, err := s.db.Exec(ctx, "DELETE FROM metric_samples WHERE sampled_at>$1 AND endpoint_id=$2", now.Add(-2*time.Second), endpoint); err != nil {
		t.Fatal(err)
	}
	id, err := queue(1, eligible)
	if err != nil || id == "" {
		t.Fatalf("current idle window was not accepted: %v", err)
	}
	reject(1, eligible, errDSGACreationConflict)
	var action, actor, selectedVM string
	if err := s.db.QueryRow(ctx, "SELECT action,actor_id,payload->>'vm_uid' FROM operations WHERE id=$1", id).
		Scan(&action, &actor, &selectedVM); err != nil || action != dsgaSuspendAction || actor != dsgaIdleActor || selectedVM != vm {
		t.Fatalf("unexpected suspension Operation: %s %s %s %v", action, actor, selectedVM, err)
	}
}

func TestDSGASuspensionAdmissionReplaysOriginalPublicOperation(t *testing.T) {
	ctx, b, _ := dsgaSuspensionAdmissionFixture(t)
	s := b.server
	s.idempotencyKey = bytes.Repeat([]byte{1}, 32)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Setenv("NEON_V2_CREATE_ENABLED", "true")
	for _, sql := range []string{
		"INSERT INTO users(id,username,password_hash,role) VALUES('suspend-admin','suspend-admin','unused','viewer')",
		"INSERT INTO organization_members(org_id,user_id,role) VALUES('local','suspend-admin','admin')",
	} {
		if _, err := s.db.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at)
 VALUES($1,'suspend-admin',$2,clock_timestamp()+interval '1 hour')`, digest("suspension-session"), digest("suspension-csrf")); err != nil {
		t.Fatal(err)
	}
	router := s.routes()
	post := func(version, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+b.intent.ProjectID+"/endpoints/"+b.intent.EndpointID+"/suspend", nil)
		req.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: "suspension-session"})
		req.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: "suspension-csrf"})
		req.Header.Set("X-CSRF-Token", "suspension-csrf")
		req.Header.Set("If-Match", version)
		req.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	t.Setenv("NEON_DSGA_SUSPENSION_ENABLED", "false")
	if w := post("", ""); w.Code != 409 || !strings.Contains(w.Body.String(), "dsga_lifecycle_not_available") {
		t.Fatalf("disabled driver admitted suspension: %d %s", w.Code, w.Body.String())
	}
	t.Setenv("NEON_DSGA_SUSPENSION_ENABLED", "true")
	if w := post("", "suspension-idempotency"); w.Code != 428 {
		t.Fatalf("missing version was accepted: %d %s", w.Code, w.Body.String())
	}
	if w := post(`"2"`, "suspension-idempotency"); w.Code != 412 {
		t.Fatalf("wrong version was accepted: %d %s", w.Code, w.Body.String())
	}
	wg := sync.WaitGroup{}
	var result [2]*httptest.ResponseRecorder
	for i := range result {
		wg.Go(func() { result[i] = post(`"1"`, "suspension-idempotency") })
	}
	wg.Wait()
	var op string
	for _, w := range result {
		var body struct{ Operation struct{ ID, Action string } }
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Operation.ID == "" || body.Operation.Action != dsgaSuspendAction {
			t.Fatalf("public operation was not accepted: %d %s", w.Code, w.Body.String())
		}
		if op != "" && body.Operation.ID != op {
			t.Fatal("concurrent replay returned a different operation")
		}
		op = body.Operation.ID
	}
	if _, err := s.db.Exec(ctx, "UPDATE operations SET payload=payload-'vm_uid' WHERE id=$1", op); err == nil {
		t.Fatal("accepted suspension identity could be changed")
	}
	if _, err := s.db.Exec(ctx, "UPDATE operations SET state='failed',retryable=true WHERE id=$1", op); err != nil {
		t.Fatal(err)
	}
	if w := post(`"1"`, "different-suspension-key"); w.Code != 409 {
		t.Fatalf("new key replaced an unresolved operation: %d %s", w.Code, w.Body.String())
	}
	if err := s.retryFailed(ctx, op, b.intent.ProjectID); err != nil {
		t.Fatal(err)
	}
	if w := post(`"1"`, "suspension-idempotency"); w.Code != 202 ||
		!strings.HasSuffix(w.Header().Get("Location"), "/"+op) {
		t.Fatalf("retry did not return original operation: %d %s", w.Code, w.Body.String())
	}
}
