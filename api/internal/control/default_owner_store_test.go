package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func defaultOwnerPayloadForTest() defaultOwnerPayload {
	writer := "ep_0123456789abcdef"
	return defaultOwnerPayload{
		catalogPayload: catalogPayload{ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", EndpointID: writer, Name: "postgres", Owner: "customer", PreviousOwner: "cloud_admin", DatabaseOID: 5, ExpectedVersion: 1},
		Native:         defaultOwnerSQLIntent{Scope: catalogNativeScope{strings.Repeat("a", 32), strings.Repeat("b", 32)}, Database: "postgres", PlatformRole: "cloud_admin", PreviousOwner: "cloud_admin", Owner: "customer", BranchID: "br_0123456789abcdef", DatabaseOID: 5, PlatformRoleOID: 10, PreviousOwnerOID: 10, OwnerOID: 12345, CommentSHA256: catalogMarkerDigest("retained arbitrary comment"), Flags: catalogDatabaseFlags{AllowConnections: true, ConnectionLimit: -1}, TargetRoleFlags: catalogRoleFlags{Login: true, Inherit: true, CreateRole: true, CreateDB: true, Replication: true, BypassRLS: true, ConnectionLimit: -1}, TargetMarkerSHA256: catalogMarkerDigest("neon-control/branch/br_0123456789abcdef")},
		Mode:           "managed", Initial: true, TargetRoleVersion: 1, TargetCredentialRef: "original-customer-credential", TargetCredentialUID: "11111111-1111-4111-8111-111111111111", BootstrapCredentialUID: "22222222-2222-4222-8222-222222222222", BootstrapVerifierSHA256: catalogMarkerDigest("original-platform-verifier"), ProbeVerifierSHA256: catalogMarkerDigest("original-probe-verifier"), TargetVerifierSHA256: catalogMarkerDigest("original-target-verifier"),
		ConfigUIDs: map[string]string{writer: "33333333-3333-4333-8333-333333333333"}, RoleHashes: map[string]string{writer: strings.Repeat("c", 64)}, ColdOwners: map[string]string{writer: ""}, RoutesUID: "44444444-4444-4444-8444-444444444444",
	}
}

func TestDefaultOwnerAtomicMetadataEscrowReplayAndFork(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgxpool.ParseConfig(dsn)
	if dsn == "" || err != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "v2_default_owner_" + randomToken(6)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgQuote(schema)); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+pgQuote(schema)+" CASCADE")
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := db.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	p := defaultOwnerPayloadForTest()
	const actor = "default_owner_admin"
	const operation = "op_111111111111111111111111"
	const worker = "default-owner-test-worker"
	const password = "private-original-platform-password"
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('default_owner_admin','default_owner_admin','unused','owner'),('default_owner_editor','default_owner_editor','unused','owner'),('default_owner_foreign','default_owner_foreign','unused','owner')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('default_owner_org','default-owner-org','Owner')`)
	exec(`INSERT INTO organization_quotas(org_id) VALUES('default_owner_org')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('default_owner_org','default_owner_admin','admin'),('default_owner_org','default_owner_editor','editor')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,tenant_id,created_at,updated_at) VALUES($1,'default_owner_org','Owner','lab',16,'ready','managed',$2,now(),now())`, p.ProjectID, p.Native.Scope.TenantID)
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,state,created_at) VALUES($1,$2,'main',$3,'ready',now())`, p.BranchID, p.ProjectID, p.Native.Scope.TimelineID)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,endpoint_type,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'active','cloud_admin','postgres','read_write',now(),now())`, p.EndpointID, p.ProjectID, p.BranchID)
	exec(`INSERT INTO branch_roles(branch_id,name,state,credential_ref) VALUES($1,'customer','ready','original-customer-credential')`, p.BranchID)
	s := &server{db: db, idempotencyKey: bytes.Repeat([]byte{3}, 32), backendKeys: &backendKeyring{Active: "v1", decoded: map[string][]byte{"v1": bytes.Repeat([]byte{7}, 32)}}}
	keyVersion, sealed, err := s.sealDefaultOwnerPassword(p.credentialScope(operation), password)
	if err != nil {
		t.Fatal(err)
	}
	expectFault := func(e error, status int) {
		t.Helper()
		var f *catalogRenameFault
		if !errors.As(e, &f) || f.Status != status {
			t.Fatalf("wrong metadata rejection %v, want %d", e, status)
		}
	}
	admit := func(value defaultOwnerPayload, who string, want int) {
		t.Helper()
		tx, e := db.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		e = admitDefaultOwnerTx(ctx, tx, value, operation, who, "original-request", "original-key", "original-hash", keyVersion, sealed)
		if want != 0 {
			expectFault(e, want)
			return
		}
		if e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
	}
	admit(p, "default_owner_editor", 403)
	admit(p, "default_owner_foreign", 403)
	exec(`UPDATE users SET disabled=true WHERE id=$1`, actor)
	admit(p, actor, 403)
	exec(`UPDATE users SET disabled=false WHERE id=$1`, actor)
	bad := p
	bad.ExpectedVersion = 2
	admit(bad, actor, 412)
	bad = p
	bad.Native.Scope.TimelineID = strings.Repeat("d", 32)
	admit(bad, actor, 409)
	bad = p
	bad.TargetRoleVersion = 2
	admit(bad, actor, 409)
	bad = p
	bad.ConfigUIDs = map[string]string{p.EndpointID: p.ConfigUIDs[p.EndpointID], "ep_1123456789abcdef": "55555555-5555-4555-8555-555555555555"}
	bad.RoleHashes = map[string]string{p.EndpointID: p.RoleHashes[p.EndpointID], "ep_1123456789abcdef": strings.Repeat("e", 64)}
	bad.ColdOwners = map[string]string{p.EndpointID: "", "ep_1123456789abcdef": ""}
	admit(bad, actor, 409)
	exec(`CREATE FUNCTION reject_owner_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'owned audit failure'; END $$; CREATE TRIGGER reject_owner_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_owner_audit()`)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = admitDefaultOwnerTx(ctx, tx, p, operation, actor, "original-request", "original-key", "original-hash", keyVersion, sealed)
	if err == nil {
		t.Fatal("audit failure accepted admission")
	}
	_ = tx.Rollback(ctx)
	var count int
	err = db.QueryRow(ctx, `SELECT (SELECT count(*) FROM default_database_owners)+(SELECT count(*) FROM default_owner_credentials)+(SELECT count(*) FROM operations)`).Scan(&count)
	if err != nil || count != 0 {
		t.Fatal("partial failed admission", err, count)
	}
	exec(`DROP TRIGGER reject_owner_audit ON audit_events`)
	admit(p, actor, 0)
	b, err := readDefaultOwnerBinding(ctx, db, p.ProjectID, p.BranchID)
	if err != nil || b.State != "updating" || b.Mode != "uninitialized" || b.Version != 1 || b.OperationID != operation {
		t.Fatal("original reservation missing", err)
	}
	expectFault(checkDefaultOwnerReservation(ctx, db, p.ProjectID, p.BranchID), 409)
	exec(`UPDATE operations SET state='failed',retryable=true,finished_at=clock_timestamp() WHERE id=$1`, operation)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,retryable) VALUES('op_333333333333333333333333',$1,'branch_catalog',$2,'rotate_role_password','failed',$3,'old-request','{}',true)`, p.ProjectID, p.BranchID, actor)
	if e := s.retryFailed(ctx, "op_333333333333333333333333", p.ProjectID); !errors.Is(e, pgx.ErrNoRows) {
		t.Fatal("unrelated old retry bypassed Owner reservation", e)
	}
	if e := s.retryFailed(ctx, operation, p.ProjectID); e != nil {
		t.Fatal("original Owner retry rejected", e)
	}
	exec(`UPDATE operations SET state='running',lease_owner=$2,lease_expires_at=clock_timestamp()+interval '5 minutes' WHERE id=$1`, operation, worker)
	commit := func(value defaultOwnerPayload, op, lease string) error {
		t.Helper()
		tx, e := db.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if e = commitDefaultOwnerResultTx(ctx, tx, op, lease, value); e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	if e := commit(p, operation, "other-worker"); e == nil {
		t.Fatal("foreign lease completed original Owner")
	}
	exec(`CREATE TRIGGER reject_owner_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_owner_audit()`)
	if e := commit(p, operation, worker); e == nil {
		t.Fatal("audit failure committed result")
	}
	err = db.QueryRow(ctx, `SELECT count(*) FROM default_owner_credentials WHERE operation_id=$1 AND cleared_at IS NULL AND ciphertext IS NOT NULL`, operation).Scan(&count)
	if err != nil || count != 1 {
		t.Fatal("failed result cleared escrow", err)
	}
	b, _ = readDefaultOwnerBinding(ctx, db, p.ProjectID, p.BranchID)
	if b.Version != 1 || b.State != "updating" {
		t.Fatal("failed result committed binding")
	}
	exec(`DROP TRIGGER reject_owner_audit ON audit_events`)
	if e := commit(p, operation, worker); e != nil {
		t.Fatal(e)
	}
	if e := commit(p, operation, worker); e != nil {
		t.Fatal("result replay failed", e)
	}
	b, err = readDefaultOwnerBinding(ctx, db, p.ProjectID, p.BranchID)
	if err != nil || b.Mode != "managed" || b.OwnerOID != 12345 || b.Version != 2 || b.State != "ready" || b.CredentialUID != p.TargetCredentialUID {
		t.Fatal("managed original result missing", err)
	}
	expectFault(checkDefaultOwnerDependency(ctx, db, p.ProjectID, p.BranchID, "role", p.Owner), 409)
	expectFault(checkDefaultOwnerDependency(ctx, db, p.ProjectID, p.BranchID, "database", p.Name), 409)
	// A live fork retains physical identity and desired Owner, under a new scope.
	const child = "br_1123456789abcdef"
	const childEndpoint = "ep_1123456789abcdef"
	childTimeline := strings.Repeat("f", 32)
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,state,created_at) VALUES($1,$2,'child',$3,'creating',now())`, child, p.ProjectID, childTimeline)
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = copyDefaultOwnerBindingTx(ctx, tx, p.ProjectID, p.BranchID, child, p.Native.Scope.TenantID, childTimeline, actor)
	if err == nil {
		err = insertEndpoint(ctx, tx, createPayload{ProjectID: p.ProjectID, BranchID: child, EndpointID: childEndpoint, TenantID: p.Native.Scope.TenantID, TimelineID: childTimeline, EndpointType: "read_write", MinCPU: 250, MaxCPU: 1000, MinMem: 512, MaxMem: 1024})
	}
	if err == nil {
		err = tx.Commit(ctx)
	} else {
		_ = tx.Rollback(ctx)
	}
	if err != nil {
		t.Fatal("live fork projection failed", err)
	}
	childBinding, err := readDefaultOwnerBinding(ctx, db, p.ProjectID, child)
	if err != nil || childBinding.DatabaseOID != 5 || childBinding.OwnerOID != 12345 || childBinding.Scope.TimelineID != childTimeline || childBinding.Basis != "live_fork" || childBinding.OperationID != "" {
		t.Fatal("fork identity/Owner changed", err)
	}
	exec(`UPDATE operations SET state='succeeded',lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, operation)
	// A second, explicit platform restoration has its own escrow and receipt.
	q := p
	q.Initial = false
	q.ExpectedVersion = 2
	q.PreviousOwner = p.Owner
	q.Owner = "cloud_admin"
	q.Native.PreviousOwner = p.Owner
	q.Native.PreviousOwnerOID = p.Native.OwnerOID
	q.Native.Owner = "cloud_admin"
	q.Native.OwnerOID = 10
	q.Native.Platform = true
	q.Mode = "platform"
	q.TargetRoleVersion = 0
	q.TargetCredentialRef = ""
	q.TargetCredentialUID = ""
	q.TargetVerifierSHA256 = ""
	q.ColdOwners = map[string]string{p.EndpointID: p.Owner}
	const restore = "op_222222222222222222222222"
	kv, blob, err := s.sealDefaultOwnerPassword(q.credentialScope(restore), password)
	if err != nil {
		t.Fatal(err)
	}
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = admitDefaultOwnerTx(ctx, tx, q, restore, actor, "restore-request", "restore-key", "restore-hash", kv, blob)
	if err == nil {
		err = tx.Commit(ctx)
	} else {
		_ = tx.Rollback(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE operations SET state='running',lease_owner=$2,lease_expires_at=clock_timestamp()+interval '5 minutes' WHERE id=$1`, restore, worker)
	if err = commit(q, restore, worker); err != nil {
		t.Fatal(err)
	}
	// Recovery of the old committed Operation must not touch native SQL or an
	// absent kube client, reopen its cleared password, or revert the newer Owner.
	exec(`UPDATE operations SET state='succeeded',lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, restore)
	exec(`UPDATE operations SET state='running',lease_owner=$2,lease_expires_at=clock_timestamp()+interval '5 minutes' WHERE id=$1`, operation, worker)
	if err = s.reconcileDefaultDatabaseOwner(ctx, operation, worker, p); err != nil {
		t.Fatal("original result recovery failed", err)
	}
	b, _ = readDefaultOwnerBinding(ctx, db, p.ProjectID, p.BranchID)
	if b.Owner != "cloud_admin" || b.Version != 3 || b.InitialOwner != "cloud_admin" {
		t.Fatal("old result reverted current Owner or initial history")
	}
	err = db.QueryRow(ctx, `SELECT count(*) FROM default_owner_credentials WHERE key_version IS NULL AND ciphertext IS NULL AND cleared_at IS NOT NULL`).Scan(&count)
	if err != nil || count != 2 {
		t.Fatal("committed credentials not cleared", err)
	}
	err = db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='update_default_database_owner' AND outcome='succeeded'`).Scan(&count)
	if err != nil || count != 2 {
		t.Fatal("duplicate/missing result audit", err)
	}
	var raw string
	err = db.QueryRow(ctx, `SELECT string_agg(payload::text,'') FROM operations`).Scan(&raw)
	if err != nil || strings.Contains(raw, password) {
		t.Fatal("plaintext in durable operation", err)
	}
	// HTTP replay returns the original receipt before looking up a retired Writer.
	input := defaultOwnerInput{Mode: "managed", Owner: p.Owner, OwnerOID: p.Native.OwnerOID, DatabaseOID: 5, PreviousOwnerOID: 10, Acknowledge: true, Initial: true, Password: password}
	body, _ := json.Marshal(input)
	path := "/api/v1/projects/" + p.ProjectID + "/branches/" + p.BranchID + "/default-database-owner"
	key := "default-owner-original-http-key"
	hash := s.createRequestHash("PATCH:"+path, record{"input": input, "version": int64(1)})
	exec(`INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)`, actor, keyHash(key), hash, operation)
	exec(`UPDATE endpoints SET deleted_at=now(),state='deleted' WHERE id=$1`, p.EndpointID)
	request := httptest.NewRequest(http.MethodPatch, path, bytes.NewReader(body))
	request.Header.Set("If-Match", `"1"`)
	request.Header.Set("Idempotency-Key", key)
	request.SetPathValue("project", p.ProjectID)
	request.SetPathValue("branch", p.BranchID)
	request = request.WithContext(context.WithValue(request.Context(), userKey, user{ID: actor}))
	response := httptest.NewRecorder()
	s.defaultDatabaseOwnerMutation(response, request)
	if response.Code != 202 || !strings.Contains(response.Body.String(), operation) || strings.Contains(response.Body.String(), password) {
		t.Fatalf("original HTTP replay failed: %d %s", response.Code, response.Body.String())
	}
}
