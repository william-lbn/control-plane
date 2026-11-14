package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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
