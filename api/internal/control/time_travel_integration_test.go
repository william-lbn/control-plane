package control

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTimeTravelPostgresPreviewAuthorityAndExactSourceAcceptance(t *testing.T) {
	ctx, s, exec := catalogBindingFixture(t)
	t.Setenv("NEON_V2_CREATE_ENABLED", "true")
	t.Setenv("NEON_V2_PITR_ENABLED", "true")
	t.Setenv("NEON_FUNCTION_BRANCH_COPY_ENABLED", "false")
	s.idempotencyKey = []byte(strings.Repeat("k", 32))
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, actor := range []string{"binding_admin", "preview_viewer", "preview_outside"} {
		if actor != "binding_admin" {
			exec(`INSERT INTO users(id,username,password_hash,role) VALUES($1,$1,'unused','viewer')`, actor)
		}
		exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, digest(actor+"-session"), actor, digest(actor+"-csrf"))
	}
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('binding_org','preview_viewer','viewer')`)
	var storageReads, leases atomic.Int64
	var changeSource atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/lsn_lease") {
			leases.Add(1)
			var body record
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["lsn"] != "0/2000" {
				t.Error("wrong accepted LSN")
			}
			jsonResponse(w, 200, record{"valid_until": "2099-01-01T00:00:00Z"})
			return
		}
		storageReads.Add(1)
		if r.Method != http.MethodGet {
			t.Error("preview changed storage")
		}
		if changeSource.Swap(false) {
			exec(`UPDATE branches SET timeline_id=$1 WHERE id='br_0123456789abcdef'`, strings.Repeat("c", 32))
		}
		jsonResponse(w, 200, record{"min_readable_lsn": "0/1000", "initdb_lsn": "0/1000", "last_record_lsn": "0/4000"})
	}))
	defer peer.Close()
	s.kube = &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}
	h := s.routes()
	request := func(actor, method, path string, body record, want int) record {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: actor + "-session"})
		r.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: actor + "-csrf"})
		r.Header.Set("X-CSRF-Token", actor+"-csrf")
		r.Header.Set("Idempotency-Key", "original-preview-acceptance")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		if method == "GET" && strings.Contains(path, "restore-point") && w.Header().Get("Cache-Control") != "no-store" && want == 200 {
			t.Fatal("preview cacheable")
		}
		var value record
		_ = json.Unmarshal(w.Body.Bytes(), &value)
		return value
	}
	base := "/api/v1/projects/prj_0123456789abcdef/branches"
	path := base + "/br_0123456789abcdef/restore-point"
	request("preview_outside", "GET", path+"?lsn=0/2000", nil, 404)
	value := request("preview_viewer", "GET", path+"?lsn=0/2000", nil, 200)
	if value["lsn"] != "0/2000" || value["source_timeline_id"] != strings.Repeat("b", 32) || value["retention_pinned"] != false || storageReads.Load() != 1 || leases.Load() != 0 {
		t.Fatal("preview acquired durable authority", value, storageReads.Load(), leases.Load())
	}
	for _, query := range []string{"", "?lsn=0/2000&lsn=0/3000", "?other=x", "?lsn=", "?timestamp=invalid"} {
		request("preview_viewer", "GET", path+query, nil, 422)
	}
	if storageReads.Load() != 1 {
		t.Fatal("invalid previews observed storage")
	}
	changeSource.Store(true)
	request("preview_viewer", "GET", path+"?lsn=0/2000", nil, 412)
	body := record{"name": "preview-fork", "parent_branch_id": "br_0123456789abcdef", "create_endpoint": false, "parent_lsn": "0/2000", "expected_parent_timeline_id": strings.Repeat("b", 32)}
	request("preview_viewer", "POST", base, body, 403)
	request("binding_admin", "POST", base, body, 412)
	if leases.Load() != 0 {
		t.Fatal("changed source acquired a lease")
	}
	body["expected_parent_timeline_id"] = strings.Repeat("c", 32)
	accepted := request("binding_admin", "POST", base, body, 202)
	op := nested(accepted, "operation", "id")
	if op == nil || leases.Load() != 1 {
		t.Fatal("exact source not accepted", accepted, leases.Load())
	}
	exec(`UPDATE branches SET timeline_id=$1 WHERE id='br_0123456789abcdef'`, strings.Repeat("d", 32))
	replay := request("binding_admin", "POST", base, body, 202)
	if nested(replay, "operation", "id") != op || leases.Load() != 1 {
		t.Fatal("original accepted point changed during replay", replay, leases.Load())
	}
	var raw []byte
	if err := s.db.QueryRow(ctx, `SELECT payload FROM operations WHERE id=$1`, op).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var payload createPayload
	if json.Unmarshal(raw, &payload) != nil || payload.ParentTimelineID != strings.Repeat("c", 32) || payload.ParentLSN != "0/2000" {
		t.Fatal("original native point not frozen")
	}
}
