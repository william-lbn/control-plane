package control

import (
	"bytes"
	"encoding/json"
	"github.com/william-lbn/control-plane/api/internal/functions"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFunctionOutputPolicyDurabilityEncryptionIsolationAndRetention(t *testing.T) {
	f := newStorageEventFixture(t)
	t.Setenv("NEON_FUNCTION_OUTPUT_ENABLED", "true")
	fn := "fnc_1123456789abcdef"
	instance := "fni_1123456789abcdef"
	boot := strings.Repeat("c", 32)
	if _, e := f.db.Exec(f.ctx, `UPDATE function_instances SET boot_id=$2 WHERE id=$1`, instance, boot); e != nil {
		t.Fatal(e)
	}
	call := func(method, suffix, key, version, body, actor string, want int) *httptest.ResponseRecorder {
		t.Helper()
		path := "/api/v1/projects/" + f.p.ProjectID + "/branches/" + f.p.BranchID + "/functions/onupload" + suffix
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Cookie", "neon_v2_session="+actor+"-session; neon_v2_csrf="+actor+"-csrf")
		r.Header.Set("X-CSRF-Token", actor+"-csrf")
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("If-Match", version)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		f.s.routes().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s %d want %d %s", method, suffix, w.Code, want, w.Body.String())
		}
		return w
	}
	before := call("GET", "/output-policy", "", "", "", "event_actor", 200)
	if !strings.Contains(before.Body.String(), `"enabled":false`) {
		t.Fatal("capture must default off")
	}
	input := `{"enabled":true,"acknowledge_sensitive_output":true}`
	call("PATCH", "/output-policy", "output-viewer-0001", `"0"`, input, "event_viewer", 403)
	call("PATCH", "/output-policy", "output-noack-00001", `"0"`, `{"enabled":true}`, "event_actor", 422)
	call("PATCH", "/output-policy", "output-nochoice-01", `"0"`, `{}`, "event_actor", 422)
	call("PATCH", "/output-policy", "output-nover-00001", "", input, "event_actor", 428)
	accepted := call("PATCH", "/output-policy", "output-original-01", `"0"`, input, "event_actor", 200)
	replay := call("PATCH", "/output-policy", "output-original-01", `"0"`, input, "event_actor", 200)
	if accepted.Body.String() != replay.Body.String() {
		t.Fatal("lost policy reply changed capture window")
	}
	call("PATCH", "/output-policy", "output-stale-0001", `"0"`, input, "event_actor", 412)
	p := functionInstanceTarget{FunctionID: fn, ProjectID: f.p.ProjectID, BranchID: f.p.BranchID, DeploymentID: "fdp_1123456789abcdef", Generation: 1, Slug: "onupload"}
	scope := functions.Scope{InstanceID: instance, DeploymentID: p.DeploymentID, ProjectID: p.ProjectID, BranchID: p.BranchID, Generation: 1, Slug: p.Slug}
	now := time.Now().UTC().Add(time.Second)
	page := functions.OutputPage{Scope: scope, BootID: boot, Next: 3, Records: []functions.OutputRecord{{Sequence: 1, Timestamp: now.Add(-time.Hour), Stream: "stdout", Severity: "INFO", ScopeName: "neon.function.app", Message: "before acknowledgment"}, {Sequence: 2, Timestamp: now, Stream: "stdout", Severity: "INFO", ScopeName: "neon.function.app", Message: "encrypted app output 中文"}, {Sequence: 3, Timestamp: now, Stream: "stderr", Severity: "ERROR", ScopeName: "neon.function.app", Message: "specific application error"}}}
	if e := f.s.storeFunctionOutputPage(f.ctx, p, instance, boot, 0, page); e != nil {
		t.Fatal(e)
	}
	if e := f.s.storeFunctionOutputPage(f.ctx, p, instance, boot, 0, page); e != nil {
		t.Fatal(e)
	}
	var count int
	var encrypted []byte
	if e := f.db.QueryRow(f.ctx, `SELECT ciphertext FROM function_output_records WHERE instance_id=$1 AND sequence=2`, instance).Scan(&encrypted); e != nil || bytes.Contains(encrypted, []byte("encrypted app output")) {
		t.Fatal("plaintext output persisted", e)
	}
	encrypted[len(encrypted)-1] ^= 1
	if _, e := f.s.openFunctionOutput(fn, instance, boot, 2, encrypted); e == nil {
		t.Fatal("tampered output accepted")
	}
	list := call("GET", "/output-logs?limit=1", "", "", "", "event_viewer", 200)
	var history struct {
		Items []record `json:"items"`
		After string   `json:"next_after"`
	}
	if json.Unmarshal(list.Body.Bytes(), &history) != nil || len(history.Items) != 1 || history.After == "" {
		t.Fatal("output pagination unavailable")
	}
	second := call("GET", "/output-logs?limit=1&after="+history.After, "", "", "", "event_viewer", 200)
	if !strings.Contains(second.Body.String(), "encrypted app output") {
		t.Fatal("second encrypted page unreadable")
	}
	filtered := call("GET", "/output-logs?severity=ERROR", "", "", "", "event_actor", 200)
	if strings.Contains(filtered.Body.String(), "encrypted app output") || !strings.Contains(filtered.Body.String(), "specific application error") {
		t.Fatal("severity filter leaked another level")
	}
	foreign := page
	foreign.Scope.BranchID = "br_2123456789abcdef"
	if e := f.s.storeFunctionOutputPage(f.ctx, p, instance, boot, 3, foreign); e == nil {
		t.Fatal("cross-branch output adopted")
	}
	disabled := `{"enabled":false,"acknowledge_sensitive_output":false}`
	overlap := page
	overlap.Next = 4
	overlap.Records = append(append([]functions.OutputRecord(nil), page.Records...), functions.OutputRecord{Sequence: 4, Timestamp: now, Stream: "stdout", Severity: "INFO", ScopeName: "neon.function.app", Message: "concurrent final shutdown suffix"})
	if e := f.s.storeFunctionOutputPage(f.ctx, p, instance, boot, 0, overlap); e != nil {
		t.Fatal(e)
	}
	var finalMessage []byte
	if f.db.QueryRow(f.ctx, `SELECT ciphertext FROM function_output_records WHERE instance_id=$1 AND sequence=4`, instance).Scan(&finalMessage) != nil {
		t.Fatal("concurrent prefix discarded final shutdown output")
	}
	call("PATCH", "/output-policy", "output-disable-01", `"1"`, disabled, "event_actor", 200)
	page.Next = 5
	page.Records = []functions.OutputRecord{{Sequence: 5, Timestamp: now, Stream: "stdout", Severity: "INFO", ScopeName: "neon.function.app", Message: "disabled must not persist"}}
	if e := f.s.storeFunctionOutputPage(f.ctx, p, instance, boot, 4, page); e != nil {
		t.Fatal(e)
	}
	if f.db.QueryRow(f.ctx, `SELECT count(*) FROM function_output_records`).Scan(&count) != nil || count != 3 {
		t.Fatal("disabled capture wrote new records")
	}
	if _, e := f.db.Exec(f.ctx, `UPDATE function_output_records SET expires_at=now()-interval '1 second'`); e != nil {
		t.Fatal(e)
	}
	expired := call("GET", "/output-logs", "", "", "", "event_actor", 200)
	if strings.Contains(expired.Body.String(), "specific application error") || !strings.Contains(expired.Body.String(), `"items":[]`) {
		t.Fatal("expired output disclosed")
	}
}
