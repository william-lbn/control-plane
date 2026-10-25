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
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type dsgaProjectFixture struct {
	t            *testing.T
	ctx          context.Context
	s            *server
	credentials  *credentialProducerAPI
	oldOperation string
	body         projectCreateRequest
}

func newDSGAProjectFixture(t *testing.T) *dsgaProjectFixture {
	t.Helper()
	ctx, s, old, _, _ := dsgaBindingFixture(t)
	f := &dsgaProjectFixture{t: t, ctx: ctx, s: s, credentials: newCredentialProducerAPI(t, "success"), oldOperation: old,
		body: projectCreateRequest{Name: "Dynamic Memory", Password: "new-project-fixture-password", DefaultBranchName: "main", PostgresVersion: 17}}
	s.kube = f.credentials.kube
	s.idempotencyKey = bytes.Repeat([]byte{0x73}, 32)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	f.exec(`INSERT INTO organizations(id,slug,name) VALUES('org_creation','creation','DSGA creation')`)
	f.exec(`INSERT INTO organization_quotas(org_id,max_projects,max_endpoints) VALUES('org_creation',5,5)`)
	for _, actor := range []string{"creation_admin", "creation_editor", "creation_viewer"} {
		f.exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','viewer')`, actor)
		f.exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,clock_timestamp()+interval '1 hour')`, digest(actor+"-session"), actor, digest(actor+"-csrf"))
		role := strings.TrimPrefix(actor, "creation_")
		f.exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_creation',$1,$2)`, actor, role)
	}
	_, policy, _ := dsgaPlanFixture(t)
	policy.VM = dsgaVMProfileFixture()
	raw, _ := json.Marshal(dsgaProjectPolicy{1, "test-region", createBounds{500, 2000, 1024, 4096}, policy})
	t.Setenv("NEON_V2_CREATE_ENABLED", "true")
	t.Setenv("NEON_DSGA_CREATE_ENABLED", "true")
	t.Setenv("NEON_DSGA_CREATION_POLICY", string(raw))
	return f
}

func (f *dsgaProjectFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.s.db.Exec(f.ctx, query, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *dsgaProjectFixture) request(actor, key string, body projectCreateRequest) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/organizations/org_creation/projects", bytes.NewReader(raw))
	r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-session"})
	r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
	r.Header.Set("X-CSRF-Token", actor+"-csrf")
	r.Header.Set("Idempotency-Key", key)
	if actor == "creation_key" {
		r.Header.Set("Authorization", "Bearer ncp_creation_fixture")
	}
	w := httptest.NewRecorder()
	f.s.routes().ServeHTTP(w, r)
	return w
}

func TestDSGAProjectOriginalSessionAndAPIKeyRecheckedAfterWrite(t *testing.T) {
	for _, kind := range []string{"session", "api_key"} {
		t.Run(kind, func(t *testing.T) {
			f := newDSGAProjectFixture(t)
			actor := "creation_admin"
			query := `UPDATE sessions SET revoked_at=clock_timestamp() WHERE token_hash=$1`
			parameter := digest("creation_admin-session")
			if kind == "api_key" {
				f.exec(`INSERT INTO api_keys(id,user_id,org_id,name,key_hash,max_role,expires_at) VALUES('creation_key','creation_admin','org_creation','test',$1,'admin',clock_timestamp()+interval '1 hour')`, digest("ncp_creation_fixture"))
				actor = "creation_key"
				query = `UPDATE api_keys SET revoked_at=clock_timestamp() WHERE id=$1`
				parameter = "creation_key"
			}
			var hookErr error
			f.credentials.mu.Lock()
			f.credentials.afterPost = func() { _, hookErr = f.s.db.Exec(f.ctx, query, parameter) }
			f.credentials.mu.Unlock()
			w := f.request(actor, "credential-revoked-after-write", f.body)
			f.credentials.mu.Lock()
			actualHookErr := hookErr
			f.credentials.afterPost = nil
			f.credentials.mu.Unlock()
			if w.Code != 403 || actualHookErr != nil {
				t.Fatal("revoked original request credential was accepted or blocked by a cross-network SQL lock")
			}
			var count int
			if err := f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM projects WHERE org_id='org_creation'`).Scan(&count); err != nil || count != 0 {
				t.Fatal("revoked original credential committed resources")
			}
		})
	}
}

func TestDSGAProjectSessionExpiresWhileWaitingForCredentialRow(t *testing.T) {
	f := newDSGAProjectFixture(t)
	f.exec(`UPDATE sessions SET expires_at=clock_timestamp()+interval '2 seconds' WHERE token_hash=$1`, digest("creation_admin-session"))
	tx, err := f.s.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	if _, err := tx.Exec(f.ctx, `SELECT token_hash FROM sessions WHERE token_hash=$1 FOR UPDATE`, digest("creation_admin-session")); err != nil {
		t.Fatal(err)
	}
	finished := make(chan int, 1)
	go func() { finished <- f.request("creation_admin", "credential-row-wait", f.body).Code }()
	waitDSGAKeyLock(t, f, "SELECT user_id,csrf_hash,expires_at,revoked_at")
	deadline := time.Now().Add(5 * time.Second)
	for {
		var expired bool
		if err := f.s.db.QueryRow(f.ctx, `SELECT clock_timestamp()>=expires_at FROM sessions WHERE token_hash=$1`, digest("creation_admin-session")).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("database credential expiry was not observed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-finished; code != 403 {
		t.Fatalf("expired row-wait credential was accepted: %d", code)
	}
	f.credentials.mu.Lock()
	defer f.credentials.mu.Unlock()
	if f.credentials.posts != 0 {
		t.Fatal("expired row-wait credential reached Secret")
	}
}

func (f *dsgaProjectFixture) reservation(key string) (string, string, string) {
	f.t.Helper()
	var op, draft, xmin string
	if err := f.s.db.QueryRow(f.ctx, `SELECT operation_id,draft::text,xmin::text FROM dsga_creation_reservations WHERE actor_id='creation_admin' AND key_hash=$1`, keyHash(key)).Scan(&op, &draft, &xmin); err != nil {
		f.t.Fatal(err)
	}
	return op, draft, xmin
}

func TestDSGAProjectPublicAcceptanceAndImmutableReplay(t *testing.T) {
	f := newDSGAProjectFixture(t)
	key := "project-original-request"
	w := f.request("creation_admin", key, f.body)
	if w.Code != 202 {
		t.Fatalf("new PG17 project status %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), f.body.Password) {
		t.Fatal("plaintext entered HTTP response")
	}
	op, draft, xmin := f.reservation(key)
	var payload []byte
	var action, state string
	var projectVersion int
	if err := f.s.db.QueryRow(f.ctx, `SELECT o.action,o.state,o.payload,p.postgres_version FROM operations o JOIN projects p ON p.id=o.project_id WHERE o.id=$1`, op).Scan(&action, &state, &payload, &projectVersion); err != nil || action != "create_project_dsga_v1" || state != "queued" || projectVersion != 17 {
		t.Fatal("public request did not persist versioned original Operation")
	}
	var accepted struct {
		createPayload
		Creation dsgaCreationPlan `json:"dsga_creation"`
	}
	if json.Unmarshal(payload, &accepted) != nil || accepted.Creation.Credentials.validate() != nil || accepted.Creation.VM.validate() != nil {
		t.Fatal("accepted original plan incomplete")
	}
	var role, database string
	var slot, steps int
	if err := f.s.db.QueryRow(f.ctx, `SELECT role_name,database_name,slot_size_mib FROM endpoints WHERE id=$1`, accepted.EndpointID).Scan(&role, &database, &slot); err != nil || role != dsgaApplicationRole || database != dsgaApplicationDatabase || slot != 1024 {
		t.Fatal("legacy superuser or slot mapping used")
	}
	if err := f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM operation_steps WHERE operation_id=$1`, op).Scan(&steps); err != nil || steps != 8 {
		t.Fatal("versioned steps incomplete")
	}
	if strings.Contains(draft, f.body.Password) || bytes.Contains(payload, []byte(f.body.Password)) || strings.Contains(draft, "SCRAM") || bytes.Contains(payload, []byte("controlSigningSeed")) {
		t.Fatal("private credential material persisted in metadata")
	}
	t.Setenv("NEON_DSGA_CREATION_POLICY", "invalid-after-original-acceptance")
	if again := f.request("creation_admin", key, f.body); again.Code != 202 {
		t.Fatal("accepted replay reloaded policy")
	}
	afterOp, afterDraft, afterXmin := f.reservation(key)
	if afterOp != op || afterDraft != draft || afterXmin != xmin {
		t.Fatal("replay rewrote original reservation")
	}
	changed := f.body
	changed.Password = "changed-project-password"
	if conflict := f.request("creation_admin", key, changed); conflict.Code != 409 {
		t.Fatal("same key accepted different original input")
	}
	f.credentials.mu.Lock()
	posts := f.credentials.posts
	f.credentials.mu.Unlock()
	if posts != 1 {
		t.Fatal("replay changed credentials")
	}
	for _, query := range []string{`UPDATE dsga_creation_reservations SET scope='foreign' WHERE operation_id=$1`, `UPDATE dsga_creation_reservations SET draft=jsonb_set(draft,'{resource_name}','"changed"') WHERE operation_id=$1`, `UPDATE dsga_creation_reservations SET accepted_id=NULL,accepted_at=NULL WHERE operation_id=$1`, `UPDATE dsga_creation_reservations SET created_at=created_at WHERE operation_id=$1`, `DELETE FROM dsga_creation_reservations WHERE operation_id=$1`, `UPDATE idempotency_keys SET request_hash=request_hash WHERE operation_id=$1`, `DELETE FROM idempotency_keys WHERE operation_id=$1`} {
		_, err := f.s.db.Exec(f.ctx, query, op)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
			t.Fatal("DB allowed immutable accepted creation change")
		}
	}
}

func TestDSGAProjectUnknownCredentialWritePreservesPlanAndQuota(t *testing.T) {
	f := newDSGAProjectFixture(t)
	key := "project-unknown-write"
	f.credentials.mu.Lock()
	f.credentials.mode = "unknown"
	f.credentials.mu.Unlock()
	if w := f.request("creation_admin", key, f.body); w.Code != 503 {
		t.Fatal("unknown credential write fabricated acceptance")
	}
	op, draft, _ := f.reservation(key)
	f.exec(`UPDATE organization_quotas SET max_projects=1,max_endpoints=1 WHERE org_id='org_creation'`)
	other := f.body
	other.Name = "Other Project"
	if w := f.request("creation_admin", "other-project-key", other); w.Code != 429 {
		t.Fatal("pending reservation escaped quota")
	}
	t.Setenv("NEON_DSGA_CREATION_POLICY", "changed-after-reservation")
	if w := f.request("creation_admin", key, f.body); w.Code != 202 {
		t.Fatalf("original recovery failed: %d %s", w.Code, w.Body.String())
	}
	afterOp, afterDraft, _ := f.reservation(key)
	if afterOp != op || afterDraft != draft {
		t.Fatal("unknown write changed original random plan or Operation")
	}
	f.credentials.mu.Lock()
	posts := f.credentials.posts
	f.credentials.mu.Unlock()
	if posts != 1 {
		t.Fatal("unknown write rotated original credentials")
	}
}

func TestDSGAProjectAuthorityRecheckedAfterExternalWrite(t *testing.T) {
	f := newDSGAProjectFixture(t)
	var hookErr error
	f.credentials.mu.Lock()
	f.credentials.afterPost = func() {
		_, hookErr = f.s.db.Exec(f.ctx, `UPDATE organization_members SET role='viewer' WHERE org_id='org_creation' AND user_id='creation_admin'`)
	}
	f.credentials.mu.Unlock()
	w := f.request("creation_admin", "project-revoked-during-write", f.body)
	f.credentials.mu.Lock()
	actualHookErr := hookErr
	f.credentials.afterPost = nil
	f.credentials.mu.Unlock()
	if w.Code != 403 || actualHookErr != nil {
		t.Fatal("external write retained SQL locks or stale authority accepted")
	}
	var count int
	if err := f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM projects WHERE org_id='org_creation'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("revoked actor created resources")
	}
	f.exec(`UPDATE organization_members SET role='admin' WHERE org_id='org_creation' AND user_id='creation_admin'`)
	if w := f.request("creation_admin", "project-revoked-during-write", f.body); w.Code != 202 {
		t.Fatal("restored authority could not reconcile original credentials")
	}
}

func TestDSGAProjectConcurrentRequestsUseOneOriginalOperation(t *testing.T) {
	f := newDSGAProjectFixture(t)
	key := "same-key-concurrent"
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- f.request("creation_admin", key, f.body).Code }()
	}
	for i := 0; i < 2; i++ {
		if code := <-results; code != 202 {
			t.Fatalf("same original concurrent request status %d", code)
		}
	}
	var resources, reservations, operations int
	if err := f.s.db.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM projects WHERE org_id='org_creation'),(SELECT count(*) FROM dsga_creation_reservations),(SELECT count(*) FROM operations WHERE action='create_project_dsga_v1')`).Scan(&resources, &reservations, &operations); err != nil || resources != 1 || reservations != 1 || operations != 1 {
		t.Fatal("concurrent request produced multiple original resources")
	}
	_, draft, _ := f.reservation(key)
	var original dsgaCreationDraft
	if json.Unmarshal([]byte(draft), &original) != nil {
		t.Fatal("original plan missing")
	}
	f.credentials.mu.Lock()
	secret := f.credentials.objects[kubeName(original.Payload.EndpointID)+"-credentials"].DeepCopy()
	f.credentials.mu.Unlock()
	snapshot, err := snapshotDSGACredentials(secret, "neon", secret.Name, original.Payload.ProjectID, original.Payload.EndpointID)
	var reference []byte
	if err != nil || f.s.db.QueryRow(f.ctx, `SELECT credential_reference FROM dsga_creation_reservations WHERE actor_id='creation_admin' AND key_hash=$1`, keyHash(key)).Scan(&reference) != nil {
		t.Fatal("actual original source unavailable")
	}
	var ref dsgaCredentialReference
	if json.Unmarshal(reference, &ref) != nil || snapshot.reference() != ref {
		t.Fatal("accepted reference differs from actual CREATE winner")
	}
}

func TestDSGAProjectReservationForwardMigrationPreservesLegacyKey(t *testing.T) {
	f := newDSGAProjectFixture(t)
	f.exec(`INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES('creation_admin',$1,$2,$3)`, keyHash("pre-migration-legacy-key"), "h1:"+strings.Repeat("e", 64), f.oldOperation)
	read := func() (string, string, uint32) {
		var row, xmin string
		var oid uint32
		if err := f.s.db.QueryRow(f.ctx, `SELECT row_to_json(i)::text,xmin::text,tableoid::oid FROM idempotency_keys i WHERE key_hash=$1`, keyHash("pre-migration-legacy-key")).Scan(&row, &xmin, &oid); err != nil {
			t.Fatal(err)
		}
		return row, xmin, oid
	}
	before, xmin, oid := read()
	f.exec(`DROP TRIGGER dsga_reserved_idempotency_guard ON idempotency_keys;
 DROP FUNCTION guard_dsga_reserved_idempotency(); DROP TABLE dsga_creation_reservations;
 DROP FUNCTION guard_dsga_creation_reservation(); DROP FUNCTION dsga_lock_creation_key(text,text);
 -- 本夹具回退到reservation表创建之前；依赖它替换守卫的前向迁移也尚不存在。
 -- 不能留下900000066已执行标记，却让重放061把新守卫换回旧v1，这不是有效的
 -- 已发布数据库快照。实际迁移DDL及标记同事务提交，不存在该半回退状态。
 DELETE FROM schema_migrations WHERE version IN (900000061,900000066)`)
	if err := migrate(f.ctx, f.s.db); err != nil {
		t.Fatal(err)
	}
	if err := migrate(f.ctx, f.s.db); err != nil {
		t.Fatal(err)
	}
	after, afterXmin, afterOID := read()
	if before != after || xmin != afterXmin || oid != afterOID {
		t.Fatal("forward migration rewrote old key or table")
	}
	if w := f.request("creation_admin", "after-forward-migration", f.body); w.Code != 202 {
		t.Fatal("forward migrated production route failed")
	}
}

func TestDSGAProjectPendingReferenceAndDescriptionCannotBeRebound(t *testing.T) {
	f := newDSGAProjectFixture(t)
	f.credentials.mu.Lock()
	f.credentials.mode = "unknown"
	f.credentials.mu.Unlock()
	key := "pending-original-facts"
	if w := f.request("creation_admin", key, f.body); w.Code != 503 {
		t.Fatal("expected unknown write")
	}
	op, _, _ := f.reservation(key)
	for _, query := range []string{
		`UPDATE dsga_creation_reservations SET credential_reference='{}' WHERE operation_id=$1`,
		`UPDATE dsga_creation_reservations SET credential_reference='{"protocol_version":1,"uid":"x"}' WHERE operation_id=$1`,
		`UPDATE dsga_creation_reservations SET operation_id='other-operation' WHERE operation_id=$1`,
		`UPDATE dsga_creation_reservations SET draft=jsonb_set(draft,'{plan,compute_instance_id}','"new-compute"') WHERE operation_id=$1`,
		`DELETE FROM dsga_creation_reservations WHERE operation_id=$1`,
	} {
		_, err := f.s.db.Exec(f.ctx, query, op)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
			t.Fatal("pending original fact was replaced or malformed reference accepted")
		}
	}
}

func TestDSGAProjectAcceptanceRejectsChangedOriginalResourceAndRecovers(t *testing.T) {
	for _, fault := range []string{"resource", "extra_step"} {
		t.Run(fault, func(t *testing.T) {
			f := newDSGAProjectFixture(t)
			key := "original-resource-guard"
			// 真实写入器故障注入：分别改变原资源ID，或混入一项已成功的计划外步骤。
			// 其余身份/payload正确，必须整笔拒绝；修复写入器后仍沿同一原计划恢复。
			timing, statement := "BEFORE", "NEW.resource_id='wrong-original-project'"
			if fault == "extra_step" {
				timing = "AFTER"
				statement = "INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES(NEW.id,99,'unplanned','succeeded')"
			}
			f.exec(`CREATE FUNCTION tamper_dsga_original_resource() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.action='create_project_dsga_v1' THEN ` + statement + `; END IF; RETURN NEW; END $$;
 CREATE TRIGGER zzz_tamper_dsga_original_resource ` + timing + ` INSERT ON operations FOR EACH ROW EXECUTE FUNCTION tamper_dsga_original_resource()`)
			if w := f.request("creation_admin", key, f.body); w.Code != 503 {
				t.Fatal("changed original resource/steps were accepted")
			}
			op, draft, _ := f.reservation(key)
			var resources, operations int
			if err := f.s.db.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM projects WHERE org_id='org_creation'),(SELECT count(*) FROM operations WHERE id=$1)`, op).Scan(&resources, &operations); err != nil || resources != 0 || operations != 0 {
				t.Fatal("failed acceptance did not rollback whole graph")
			}
			f.exec(`DROP TRIGGER zzz_tamper_dsga_original_resource ON operations; DROP FUNCTION tamper_dsga_original_resource()`)
			if w := f.request("creation_admin", key, f.body); w.Code != 202 {
				t.Fatal("original source/Operation did not recover after writer repair")
			}
			afterOp, afterDraft, _ := f.reservation(key)
			if op != afterOp || draft != afterDraft {
				t.Fatal("failed accept reallocated original plan")
			}
			f.credentials.mu.Lock()
			defer f.credentials.mu.Unlock()
			if f.credentials.posts != 1 {
				t.Fatal("failed metadata transaction changed credentials")
			}
		})
	}
}

func TestDSGAProjectPendingKeyConflictsWithLegacyInBothDirections(t *testing.T) {
	for _, winner := range []string{"legacy", "reservation"} {
		t.Run(winner, func(t *testing.T) {
			f := newDSGAProjectFixture(t)
			key := "cross-driver-key"
			actor := "creation_admin"
			tx, err := f.s.db.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(f.ctx) }()
			insertLegacy := `INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`
			legacyHash := "h1:" + strings.Repeat("a", 64)
			if winner == "legacy" {
				if _, err := tx.Exec(f.ctx, insertLegacy, actor, keyHash(key), legacyHash, f.oldOperation); err != nil {
					t.Fatal(err)
				}
				finished := make(chan int, 1)
				go func() { finished <- f.request(actor, key, f.body).Code }()
				waitDSGAKeyLock(t, f, "INSERT INTO dsga_creation_reservations")
				if err := tx.Commit(f.ctx); err != nil {
					t.Fatal(err)
				}
				if code := <-finished; code != 409 {
					t.Fatalf("reservation did not observe committed legacy winner: %d", code)
				}
			} else {
				p, plan := dsgaVMFixture(t)
				plan.Credentials = dsgaCredentialReference{}
				draft, _ := json.Marshal(dsgaCreationDraft{1, f.body.Name, "main", "test-region", p, plan})
				if _, err := tx.Exec(f.ctx, `INSERT INTO dsga_creation_reservations(actor_id,key_hash,request_hash,scope,org_id,base_action,operation_id,draft)
 VALUES($1,$2,$3,$4,'org_creation','create_project','op_reserved_cross',$5)`, actor, keyHash(key), f.s.createRequestHash("POST:/organizations/org_creation/projects", f.body), "POST:/organizations/org_creation/projects", draft); err != nil {
					t.Fatal(err)
				}
				finished := make(chan error, 1)
				go func() {
					_, err := f.s.db.Exec(f.ctx, insertLegacy, actor, keyHash(key), legacyHash, f.oldOperation)
					finished <- err
				}()
				waitDSGAKeyLock(t, f, "INSERT INTO idempotency_keys")
				if err := tx.Commit(f.ctx); err != nil {
					t.Fatal(err)
				}
				var pgerr *pgconn.PgError
				if err := <-finished; !errors.As(err, &pgerr) || pgerr.Code != "23505" {
					t.Fatal("legacy did not observe committed reservation winner")
				}
			}
			f.credentials.mu.Lock()
			posts := f.credentials.posts
			f.credentials.mu.Unlock()
			if posts != 0 {
				t.Fatal("cross-driver key conflict reached external credentials")
			}
		})
	}
}

func waitDSGAKeyLock(t *testing.T, f *dsgaProjectFixture, fragment string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := f.s.db.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=current_setting('application_name') AND wait_event_type='Lock' AND query LIKE $1)`, "%"+fragment+"%").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("actual cross-driver advisory wait was not observed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDSGAProjectDefaultGateAndNoExternalWritesForInvalidRequest(t *testing.T) {
	f := newDSGAProjectFixture(t)
	t.Setenv("NEON_DSGA_CREATE_ENABLED", "false")
	if w := f.request("creation_admin", "default-disabled-key", f.body); w.Code != 503 {
		t.Fatal("PG17 creation not disabled")
	}
	t.Setenv("NEON_DSGA_CREATE_ENABLED", "true")
	if w := f.request("creation_viewer", "viewer-project-key", f.body); w.Code != 403 {
		t.Fatal("viewer created project")
	}
	body := f.body
	body.Password = "short"
	if w := f.request("creation_admin", "invalid-password-key", body); w.Code != 422 {
		t.Fatal("invalid password reached reservation")
	}
	body = f.body
	body.Autoscaling = &createBounds{500, 2147483750, 1024, 4096}
	if w := f.request("creation_admin", "invalid-int-bounds-key", body); w.Code != 422 {
		t.Fatal("unpersistable CPU bounds were accepted")
	}
	t.Setenv("NEON_DSGA_CREATION_POLICY", `{"protocol_version":1,"region":"test","bounds":{"min_cpu_milli":-1}}`)
	if w := f.request("creation_admin", "invalid-platform-policy", f.body); w.Code != 503 {
		t.Fatal("invalid trusted policy was accepted or blamed on client")
	}
	var count int
	if err := f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM dsga_creation_reservations`).Scan(&count); err != nil || count != 0 {
		t.Fatal("invalid request created reservation")
	}
	f.credentials.mu.Lock()
	defer f.credentials.mu.Unlock()
	if f.credentials.posts != 0 {
		t.Fatal("invalid request created external credentials")
	}
}
