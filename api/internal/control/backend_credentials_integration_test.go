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
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBackendCredentialsIntegration(t *testing.T) {
	url := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgxpool.ParseConfig(url)
	if url == "" || err != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("disposable control_ci PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "v2_backend_" + randomToken(6)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"alice", "bob", "viewer", "outsider"} {
		exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','owner')`, name)
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(name+"-session"), name, digest(name+"-csrf"))
	}
	exec(`INSERT INTO organizations(id,slug,name) VALUES('org_a','a','A'),('org_b','b','B')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_a','alice','admin'),('org_a','bob','editor'),('org_a','viewer','viewer'),('org_b','outsider','admin')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES('prj_a','org_a','A','lab',16,'ready','managed',now(),now()),('prj_b','org_b','B','lab',16,'ready','managed',now(),now())`)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at,parent_branch_id) VALUES('br_main','prj_a','main','ready',now(),NULL),('br_other','prj_b','main','ready',now(),NULL)`)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at,parent_branch_id) VALUES('br_child','prj_a','child','ready',now(),'br_main'),('br_sibling','prj_a','sibling','ready',now(),'br_main')`)
	exec(`INSERT INTO branches(id,project_id,name,state,created_at,parent_branch_id) VALUES('br_grand','prj_a','grand','ready',now(),'br_child')`)
	s := &server{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), idempotencyKey: bytes.Repeat([]byte{0x42}, 32), backendKeys: &backendKeyring{Active: "v1", decoded: map[string][]byte{"v1": bytes.Repeat([]byte{0x31}, 32), "v2": bytes.Repeat([]byte{0x32}, 32)}}}
	handler := s.routes()
	call := func(actor, method, path string, body any, key, version string, want int) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-session"})
		req.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
		req.Header.Set("X-CSRF-Token", actor+"-csrf")
		req.Header.Set("Idempotency-Key", key)
		req.Header.Set("If-Match", version)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != want {
			t.Fatalf("%s %s status %d, expected %d; response omitted to protect one-time secrets", method, path, response.Code, want)
		}
		var value map[string]any
		if json.Unmarshal(response.Body.Bytes(), &value) != nil {
			t.Fatal("invalid JSON response")
		}
		return value
	}
	base := "/api/v1/projects/prj_a/branches/"
	t.Run("storage signing issue replay lineage rotation and revoke", func(t *testing.T) {
		storageSpec := backendCredentialSpec{Name: "storage-reader", Scopes: []string{"storage:read"}, BranchScope: "self_and_descendants", ExpiresAt: time.Now().UTC().Add(time.Hour), AllowedModels: []string{}}
		issued := call("alice", "POST", base+"br_main/credentials", storageSpec, "s3-issue-original", "", 201)
		id := issued["credential"].(map[string]any)["id"].(string)
		token := issued["api_token"].(string)
		if issued["token_id"] != id || issued["s3_secret_access_key"] != storageSigningSecret(token) {
			t.Fatal("S3 one-time signing identity mismatch")
		}
		sealedToken, anchor, e := s.loadStorageSigning(ctx, id)
		if e != nil || anchor != "br_main" || sealedToken != token {
			t.Fatal("authenticated storage envelope unavailable")
		}
		if _, e = s.verifyBackendCredential(ctx, token, "br_child", "storage:read", ""); e != nil {
			t.Fatal("descendant read denied")
		}
		if _, e = s.verifyBackendCredential(ctx, token, "br_child", "storage:write", ""); e == nil {
			t.Fatal("read credential allowed write")
		}
		replay := call("alice", "POST", base+"br_main/credentials", storageSpec, "s3-issue-original", "", 200)
		if replay["api_token"] != nil || replay["s3_secret_access_key"] != nil {
			t.Fatal("durable replay revealed signing secret")
		}
		storageSpec.Scopes = []string{"storage:write"}
		rotated := call("alice", "POST", base+"br_main/credentials/"+id+"/rotate", storageSpec, "s3-rotate-original", `"1"`, 200)
		nextToken := rotated["api_token"].(string)
		if rotated["token_id"] != id || nextToken == token || storageSigningSecret(nextToken) == storageSigningSecret(token) {
			t.Fatal("rotation did not replace both secrets in place")
		}
		if _, e = s.verifyBackendCredential(ctx, token, "br_main", "storage:read", ""); e == nil {
			t.Fatal("old token valid after rotation")
		}
		if _, e = s.verifyBackendCredential(ctx, nextToken, "br_child", "storage:read", ""); e != nil {
			t.Fatal("write scope must include reads")
		}
		current, _, e := s.loadStorageSigning(ctx, id)
		if e != nil || current != nextToken {
			t.Fatal("rotated ciphertext generation mismatch")
		}
		var encrypted []byte
		var replies string
		if db.QueryRow(ctx, `SELECT sealed_token FROM backend_storage_signing WHERE credential_id=$1`, id).Scan(&encrypted) != nil || db.QueryRow(ctx, `SELECT string_agg(response::text,',') FROM backend_credential_requests`).Scan(&replies) != nil {
			t.Fatal("storage secrecy check unavailable")
		}
		if bytes.Contains(encrypted, []byte(token)) || bytes.Contains(encrypted, []byte(nextToken)) || strings.Contains(replies, token) || strings.Contains(replies, nextToken) || strings.Contains(replies, storageSigningSecret(nextToken)) {
			t.Fatal("cleartext storage signing material persisted")
		}
		call("alice", "DELETE", base+"br_main/credentials/"+id, nil, "s3-revoke-original", `"2"`, 200)
		if _, _, e = s.loadStorageSigning(ctx, id); e == nil {
			t.Fatal("revoked signing material still admitted")
		}
	})
	consoleRequest := map[string]any{"method": "GET", "table": "notes", "application_token": "not-a-real-token"}
	call("viewer", "POST", base+"br_main/data-api/request", consoleRequest, "", "", 403)
	call("outsider", "POST", base+"br_main/data-api/request", consoleRequest, "", "", 404)
	call("alice", "POST", base+"br_other/data-api/request", consoleRequest, "", "", 404)
	call("alice", "POST", base+"br_main/data-api/request", map[string]any{"method": "GET", "table": "https://evil.example/notes", "application_token": "placeholder"}, "", "", 422)
	spec := backendCredentialSpec{Name: "server", Scopes: []string{"ai_gateway:invoke"}, BranchScope: "self_and_descendants", ExpiresAt: time.Now().UTC().Add(24 * time.Hour), AllowedModels: []string{"model-a"}}
	rootReply := call("alice", "POST", base+"br_main/credentials", spec, "root-credential-1", "", 201)
	rootToken := rootReply["api_token"].(string)
	rootID := rootReply["credential"].(map[string]any)["id"].(string)
	replay := call("alice", "POST", base+"br_main/credentials", spec, "root-credential-1", "", 200)
	if replay["api_token"] != nil || replay["secret_available"] != false {
		t.Fatal("replay revealed token")
	}
	changed := spec
	changed.Name = "changed"
	call("alice", "POST", base+"br_main/credentials", changed, "root-credential-1", "", 409)
	for _, branch := range []string{"br_main", "br_child", "br_grand", "br_sibling"} {
		if _, err = s.verifyBackendCredential(ctx, rootToken, branch, "ai_gateway:invoke", "model-a"); err != nil {
			t.Fatal("root lineage rejected:", branch)
		}
	}
	for _, input := range [][3]string{{"br_other", "ai_gateway:invoke", "model-a"}, {"br_main", "object_storage:read", "model-a"}, {"br_main", "ai_gateway:invoke", "model-b"}} {
		if _, err = s.verifyBackendCredential(ctx, rootToken, input[0], input[1], input[2]); !errors.Is(err, errBackendCredentialForbidden) {
			t.Fatal("scope isolation failed")
		}
	}
	childReply := call("bob", "POST", base+"br_child/credentials", spec, "child-credential-1", "", 201)
	childToken := childReply["api_token"].(string)
	childID := childReply["credential"].(map[string]any)["id"].(string)
	for _, branch := range []string{"br_main", "br_sibling"} {
		if _, err = s.verifyBackendCredential(ctx, childToken, branch, "ai_gateway:invoke", ""); !errors.Is(err, errBackendCredentialForbidden) {
			t.Fatal("child escaped lineage")
		}
	}
	if _, err = s.verifyBackendCredential(ctx, childToken, "br_grand", "ai_gateway:invoke", ""); err != nil {
		t.Fatal("grandchild rejected")
	}
	self := spec
	self.BranchScope = "self"
	selfReply := call("alice", "POST", base+"br_main/credentials", self, "self-credential-1", "", 201)
	if _, err = s.verifyBackendCredential(ctx, selfReply["api_token"].(string), "br_child", "ai_gateway:invoke", ""); !errors.Is(err, errBackendCredentialForbidden) {
		t.Fatal("self-only credential reached child")
	}
	call("viewer", "POST", base+"br_main/credentials", spec, "viewer-credential", "", 403)
	call("outsider", "GET", base+"br_main/credentials", nil, "", "", 404)
	call("alice", "POST", base+"br_other/credentials/check", map[string]string{"api_token": rootToken}, "", "", 404)
	call("bob", "DELETE", base+"br_main/credentials/"+rootID, nil, "bob-revoke-root", `"1"`, 403)
	// Backend tokens must never authenticate Console control APIs.
	req := httptest.NewRequest("GET", "/api/v1/projects/prj_a", nil)
	req.Header.Set("Authorization", "Bearer "+rootToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != 401 {
		t.Fatal("Backend token authenticated control API")
	}
	// A separate API process sees the same current DB revocation/role state.
	peer := &server{db: db, backendKeys: s.backendKeys}
	rotated := call("alice", "POST", base+"br_main/credentials/"+rootID+"/rotate", spec, "rotate-root-1", `"1"`, 200)
	if _, err = peer.verifyBackendCredential(ctx, rootToken, "br_main", "ai_gateway:invoke", ""); !errors.Is(err, errBackendCredentialInvalid) {
		t.Fatal("old token survived rotation")
	}
	newToken := rotated["api_token"].(string)
	if _, err = peer.verifyBackendCredential(ctx, newToken, "br_main", "ai_gateway:invoke", ""); err != nil {
		t.Fatal("rotated token rejected")
	}
	call("alice", "POST", base+"br_main/credentials/"+rootID+"/rotate", spec, "stale-rotate", `"1"`, 412)
	call("alice", "DELETE", base+"br_main/credentials/"+rootID, nil, "revoke-root-1", `"2"`, 200)
	if _, err = peer.verifyBackendCredential(ctx, newToken, "br_main", "ai_gateway:invoke", ""); !errors.Is(err, errBackendCredentialInvalid) {
		t.Fatal("revoked token survived peer verification")
	}
	call("alice", "POST", base+"br_main/credentials/"+rootID+"/rotate", spec, "rotate-revoked", `"3"`, 409)
	exec(`UPDATE organization_members SET role='viewer' WHERE org_id='org_a' AND user_id='bob'`)
	if _, err = peer.verifyBackendCredential(ctx, childToken, "br_child", "ai_gateway:invoke", ""); !errors.Is(err, errBackendCredentialForbidden) {
		t.Fatal("downgraded user token retained permission")
	}
	exec(`UPDATE organization_members SET role='editor' WHERE org_id='org_a' AND user_id='bob'`)
	exec(`UPDATE backend_credentials SET expires_at=now()-interval '1 second' WHERE id=$1`, childID)
	if _, err = peer.verifyBackendCredential(ctx, childToken, "br_child", "ai_gateway:invoke", ""); !errors.Is(err, errBackendCredentialInvalid) {
		t.Fatal("expired token accepted")
	}
	// Activation of a new pepper keeps historical credential verification and
	// replay identities stable while new tokens use the active key version.
	s.backendKeys.Active = "v2"
	call("alice", "POST", base+"br_main/credentials", spec, "root-credential-1", "", 200)
	if _, err = peer.verifyBackendCredential(ctx, selfReply["api_token"].(string), "br_main", "ai_gateway:invoke", ""); err != nil {
		t.Fatal("old pepper credential lost")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := map[string]bool{}
	secrets := 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, _ := json.Marshal(spec)
			req := httptest.NewRequest("POST", base+"br_main/credentials", bytes.NewReader(raw))
			req.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: "alice-session"})
			req.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: "alice-csrf"})
			req.Header.Set("X-CSRF-Token", "alice-csrf")
			req.Header.Set("Idempotency-Key", "concurrent-credential-1")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			var reply map[string]any
			if response.Code != 200 && response.Code != 201 {
				t.Error("concurrent request failed")
				return
			}
			if json.Unmarshal(response.Body.Bytes(), &reply) != nil {
				t.Error("concurrent reply invalid")
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ids[reply["credential"].(map[string]any)["id"].(string)] = true
			if reply["api_token"] != nil {
				secrets++
			}
		}()
	}
	wg.Wait()
	if len(ids) != 1 || secrets != 1 {
		t.Fatal("concurrent idempotency created duplicate identity or secret")
	}
	var rows, requests string
	if err = db.QueryRow(ctx, `SELECT string_agg(token_hash,','),(SELECT string_agg(response::text,',') FROM backend_credential_requests) FROM backend_credentials`).Scan(&rows, &requests); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{rootToken, newToken, childToken} {
		if strings.Contains(rows, secret) || strings.Contains(requests, secret) {
			t.Fatal("plaintext credential stored in metadata")
		}
	}
}
