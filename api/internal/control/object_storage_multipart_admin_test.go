package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMultipartAdministrationIntentAuditRecoveryAuthorizationAndReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cfg, e := pgxpool.ParseConfig(os.Getenv("NEON_V2_TEST_DATABASE_URL"))
	if e != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("Disposable control_ci required")
	}
	admin, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	schema := "v2_multipart_admin_" + randomToken(6)
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = migrate(ctx, db); e != nil {
		t.Fatal(e)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := db.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	for _, actor := range []string{"admin", "editor", "viewer", "outsider"} {
		exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','owner')`, actor)
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(actor+"-token"), actor, digest(actor+"-csrf"))
	}
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org_multipart_admin','multipart-admin','Multipart admin')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_multipart_admin','admin','owner'),('org_multipart_admin','editor','editor'),('org_multipart_admin','viewer','viewer')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES('prj_multipart_admin','org_multipart_admin','Multipart admin','lab',16,'ready','managed',now(),now())`)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES('br_multipart_admin','prj_multipart_admin','main','ready',now())`)
	s := &server{db: db, idempotencyKey: bytes.Repeat([]byte{7}, 32)}
	const upload = "mpu_0123456789abcdef01234567"
	const base = "/api/v1/projects/prj_multipart_admin/branches/br_multipart_admin/storage/buckets/ci-multipart/multipart-administration"
	const key = "original-admin-abort-attempt"
	const reason = "Revoked credential abandoned this upload"
	call := func(actor, method, path string, want int) record {
		t.Helper()
		raw, _ := json.Marshal(record{"reason": reason})
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-token"})
		r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
		r.Header.Set("X-CSRF-Token", actor+"-csrf")
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s got %d wanted %d: %s", actor, method, w.Code, want, w.Body.String())
		}
		var value record
		if json.Unmarshal(w.Body.Bytes(), &value) != nil {
			t.Fatal("JSON response required")
		}
		return value
	}
	// The historical status schema still fixed s3_compatible=false while the
	// actual SigV4 adapter could be enabled. Check the real HTTP shape against
	// its closed contract, including the new separate Console capability.
	t.Setenv("NEON_S3_PUBLIC_ORIGIN", "http://192.0.2.1:30893")
	s.storage = &storageBlobs{}
	s.backendKeys = &backendKeyring{}
	statusBody := call("admin", "GET", "/api/v1/projects/prj_multipart_admin/branches/br_multipart_admin/storage", 200)
	statusSchema := loadOpenAPI(t)["components"].(map[string]any)["schemas"].(map[string]any)["ObjectStorageInstance"].(map[string]any)
	properties := statusSchema["properties"].(map[string]any)
	if statusBody["s3_compatible"] != true || properties["s3_compatible"].(map[string]any)["const"] != nil || statusSchema["additionalProperties"] != false {
		t.Fatal("configured S3 status contradicts schema")
	}
	for field := range statusBody {
		if properties[field] == nil {
			t.Fatal("actual status field missing from contract", field)
		}
	}
	for _, field := range statusSchema["required"].([]any) {
		if _, ok := statusBody[field.(string)]; !ok {
			t.Fatal("required status field missing", field)
		}
	}
	s.storage = nil
	s.backendKeys = nil
	for _, actor := range []string{"editor", "viewer"} {
		call(actor, "GET", base, 403)
		call(actor, "POST", base+"/"+upload+"/abort", 403)
	}
	call("outsider", "GET", base, 404)
	request := func(reason string) *http.Request {
		r := httptest.NewRequest("POST", base+"/"+upload+"/abort", nil)
		r.SetPathValue("project", "prj_multipart_admin")
		r.SetPathValue("branch", "br_multipart_admin")
		r.SetPathValue("bucket", "ci-multipart")
		r.SetPathValue("upload", upload)
		return r.WithContext(context.WithValue(context.WithValue(ctx, userKey, user{ID: "admin"}), requestIDKey, "original-admin-request"))
	}
	apiKey := request(reason)
	apiKey = apiKey.WithContext(context.WithValue(context.WithValue(apiKey.Context(), userKey, user{ID: "admin", APIKey: true}), resourceAccessKey{}, resourceAccess{Level: 3}))
	denied := httptest.NewRecorder()
	s.storageMultipartAdmin(denied, apiKey)
	if denied.Code != 403 {
		t.Fatal("API key bypassed privileged console admission")
	}
	// Intent admission must fail closed if the durable audit cannot commit.
	exec(`ALTER TABLE audit_events ADD CONSTRAINT ci_deny_admission CHECK (action<>'admit_multipart_admin_abort')`)
	if _, e = s.admitMultipartAdmin(request(reason), key, reason); e == nil {
		t.Fatal("audit failure ignored")
	}
	var n int
	if e = db.QueryRow(ctx, `SELECT count(*) FROM object_multipart_admin_requests`).Scan(&n); e != nil || n != 0 {
		t.Fatal("unaudited intent survived")
	}
	exec(`ALTER TABLE audit_events DROP CONSTRAINT ci_deny_admission`)
	// A real routed admin request commits its intent before the intentionally
	// unavailable data driver. No fake completed receipt is returned.
	call("admin", "POST", base+"/"+upload+"/abort", 503)
	intent, e := s.admitMultipartAdmin(request(reason), key, reason)
	if e != nil || intent.State != "admitted" {
		t.Fatal("admitted intent not recoverable", e)
	}
	replay, e := s.admitMultipartAdmin(request(reason), key, reason)
	if e != nil || replay.ID != intent.ID {
		t.Fatal("intent replay differs", e)
	}
	if _, e = s.admitMultipartAdmin(request(reason), key, reason+" changed"); e == nil {
		t.Fatal("different reason replay accepted")
	}
	if e = db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='admit_multipart_admin_abort'`).Scan(&n); e != nil || n != 1 {
		t.Fatal("admission audit duplicated")
	}
	// Model a branch abort whose response was lost. Failed outcome auditing
	// leaves the original admitted intent, then exact retry converges once.
	exec(`ALTER TABLE audit_events ADD CONSTRAINT ci_deny_confirmation CHECK (action<>'confirm_multipart_admin_abort')`)
	value := record{"upload_id": upload, "state": "aborted", "initiator": "credential:retired", "physical_gc": "held_for_branch_and_pitr_references"}
	if _, _, e = s.confirmMultipartAdmin(request(reason), intent, 200, value); e == nil {
		t.Fatal("confirmation audit failure ignored")
	}
	var state string
	if e = db.QueryRow(ctx, `SELECT state FROM object_multipart_admin_requests WHERE id=$1`, intent.ID).Scan(&state); e != nil || state != "admitted" {
		t.Fatal("failed confirmation committed partial receipt")
	}
	exec(`ALTER TABLE audit_events DROP CONSTRAINT ci_deny_confirmation`)
	first, status, e := s.confirmMultipartAdmin(request(reason), intent, 200, value)
	if e != nil || status != 200 {
		t.Fatal(e)
	}
	again, status, e := s.confirmMultipartAdmin(request(reason), intent, 409, record{"state": "changed"})
	if e != nil || status != 200 || !bytes.Equal(first, again) {
		t.Fatal("confirmed receipt changed")
	}
	actual := call("admin", "POST", base+"/"+upload+"/abort", 200)
	if actual["intent_id"] != intent.ID || actual["state"] != "aborted" {
		t.Fatal("route did not replay without waking disabled driver")
	}
	if e = db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='confirm_multipart_admin_abort'`).Scan(&n); e != nil || n != 1 {
		t.Fatal("completion audit duplicated")
	}
	for _, bad := range []string{"short", " trailing reason ", "newline\nreason", strings.Repeat("x", 513)} {
		if validMultipartReason(bad) {
			t.Fatal("invalid reason accepted")
		}
	}
	// Only new admissions count against the 60/hour bound, not exact retries.
	exec(`INSERT INTO object_multipart_admin_requests(id,actor_id,project_id,branch_id,bucket,upload_id,reason,key_hash,request_hash,request_id) SELECT 'rate-'||i,'admin','prj_multipart_admin','br_multipart_admin','ci-multipart',$1,'Rate fixture','rate-'||i,'fixture','fixture' FROM generate_series(1,59) i`, upload)
	_, e = s.admitMultipartAdmin(request(reason), "new-over-limit-attempt", reason)
	fault, ok := e.(*storageMutationFault)
	if !ok || fault.Code != "AdminRateLimited" {
		t.Fatal("rate bound not enforced", e)
	}
}
