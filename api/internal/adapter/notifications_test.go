package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func attachPayload(node int) string {
	raw, _ := json.Marshal(map[string]any{"tenant_id": testTenant, "shards": []any{map[string]any{"node_id": node, "shard_number": 0}}, "preferred_az": nil, "stripe_size": nil})
	return string(raw)
}
func keepersPayload(generation string) string {
	return `{"tenant_id":"` + testTenant + `","timeline_id":"` + testTimeline + `","generation":` + generation + `,"safekeepers":[{"id":1,"hostname":null},{"id":2,"hostname":null},{"id":3,"hostname":null}]}`
}
func TestNativeNotificationInputAndPlacement(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		status           int
	}{
		{"native_attach", "/notify-attach", attachPayload(2), 200},
		{"unsupported_move", "/notify-attach", attachPayload(3), 423},
		{"native_safekeepers", "/notify-safekeepers", keepersPayload("1"), 200},
		{"exact_uint64", "/notify-safekeepers", keepersPayload("18446744073709551615"), 200},
		{"invalid_fraction_generation", "/notify-safekeepers", keepersPayload("1.2"), 400},
		{"invalid_negative_generation", "/notify-safekeepers", keepersPayload("-1"), 400},
		{"invalid_generation_overflow", "/notify-safekeepers", keepersPayload("18446744073709551616"), 400},
		{"invalid_tenant", "/notify-attach", `{"tenant_id":"nothexnothexnothexnothexnothexno","shards":[{"node_id":2,"shard_number":0}]}`, 400},
		{"empty_shards", "/notify-attach", `{"tenant_id":"` + testTenant + `","shards":[]}`, 400},
		{"duplicate_shard", "/notify-attach", `{"tenant_id":"` + testTenant + `","shards":[{"node_id":2,"shard_number":0},{"node_id":2,"shard_number":0}]}`, 400},
		{"malformed_json", "/notify-attach", `{`, 400},
		{"trailing_json", "/notify-attach", attachPayload(2) + `{}`, 400},
		{"oversize", "/notify-attach", strings.Repeat("x", 64001), 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, _ := fixture(t)
			w := request(s, "PUT", tc.path, "hook-test-credential", tc.body)
			if w.Code != tc.status {
				t.Fatalf("status %d expected %d", w.Code, tc.status)
			}
			if f.configmap["data"].(map[string]any)["unrelated"] != "retained" {
				t.Fatal("unrelated state lost")
			}
		})
	}
}
func TestLegacyReceiptCanonicalReplayAndMonotonicGeneration(t *testing.T) {
	s, f, _ := fixture(t)
	// A pre-migration Python hash differs due to JSON whitespace. Exact payload
	// semantics, including >2^53 integers, must survive the Go migration.
	payload, err := notification([]byte(keepersPayload("9007199254740993")), "safekeepers")
	if err != nil {
		t.Fatal(err)
	}
	key := "safekeepers:" + testTenant + ":" + testTimeline
	old := map[string]receipt{key: {Hash: "python-legacy-hash", Payload: payload, ReceivedAt: 1791370000}}
	raw, _ := json.Marshal(old)
	f.configmap["data"].(map[string]any)["receipts.json"] = string(raw)
	before := string(raw)
	for _, generation := range []string{"9007199254740993", "9007199254740992"} {
		w := request(s, "PUT", "/notify-safekeepers", "hook-test-credential", keepersPayload(generation))
		if w.Code != 200 {
			t.Fatalf("replay/stale rejected: %d", w.Code)
		}
	}
	if f.configmap["data"].(map[string]any)["receipts.json"] != before {
		t.Fatal("replay/stale receipt rewritten")
	}
	if f.calls["PUT "+s.path("configmaps", s.config.NotificationsConfigMap)] != 0 {
		t.Fatal("idempotent replay mutated")
	}
	changed := strings.Replace(keepersPayload("9007199254740993"), `"id":3`, `"id":4`, 1)
	if request(s, "PUT", "/notify-safekeepers", "hook-test-credential", changed).Code != 503 {
		t.Fatal("conflicting same generation acknowledged")
	}
	if request(s, "PUT", "/notify-safekeepers", "hook-test-credential", keepersPayload("9007199254740994")).Code != 423 {
		t.Fatal("unsupported live reconfiguration acknowledged")
	}
}
func TestReceiptConflictRetriesWithNewResourceVersion(t *testing.T) {
	s, f, _ := fixture(t)
	conflicts := 0
	f.on = func(ctx context.Context, method, path string, body any) (map[string]any, error, bool) {
		if method == http.MethodPut && strings.Contains(path, "/configmaps/") && conflicts < 2 {
			conflicts++
			f.configmap["metadata"].(map[string]any)["resourceVersion"] = "updated"
			return nil, APIError{409}, true
		}
		return nil, nil, false
	}
	w := request(s, "PUT", "/notify-attach", "hook-test-credential", attachPayload(2))
	if w.Code != 200 || conflicts != 2 {
		t.Fatal("CAS recovery failed")
	}
	if f.configmap["metadata"].(map[string]any)["resourceVersion"] != "updated" {
		t.Fatal("stale version update")
	}
	if f.configmap["data"].(map[string]any)["unrelated"] != "retained" {
		t.Fatal("other ConfigMap state reset")
	}
	w = request(s, "PUT", "/notify-attach", "hook-test-credential", attachPayload(2))
	if w.Code != 200 {
		t.Fatal("attach replay failed")
	}
	if f.calls["PUT "+s.path("configmaps", s.config.NotificationsConfigMap)] != 3 {
		t.Fatal("attach replay did not stay read-only")
	}
}
func TestNotificationFailsClosedOnAuthoritativeAndWriteErrors(t *testing.T) {
	for _, kind := range []string{"unknown_controller", "wrong_controller_placement", "wrong_existing_route", "uncertain_put", "persistent_conflict", "capacity"} {
		t.Run(kind, func(t *testing.T) {
			s, f, _ := fixture(t)
			if kind == "wrong_existing_route" {
				routeValue(f)["pageserver_node_id"] = 3
			}
			if kind == "capacity" {
				payload, _ := notification([]byte(keepersPayload("1")), "safekeepers")
				payload["padding"] = strings.Repeat("x", 520<<10)
				raw, _ := json.Marshal(map[string]receipt{"retained-old": {Hash: "old", Payload: payload}})
				f.configmap["data"].(map[string]any)["receipts.json"] = string(raw)
			}
			f.on = func(ctx context.Context, method, path string, body any) (map[string]any, error, bool) {
				if strings.Contains(path, "/services/") {
					if kind == "unknown_controller" {
						return map[string]any{}, nil, true
					}
					if kind == "wrong_controller_placement" {
						return map[string]any{"shards": []any{map[string]any{"node_attached": float64(3)}}}, nil, true
					}
				}
				if method == http.MethodPut {
					if kind == "uncertain_put" {
						return nil, errors.New("connection lost after mutation"), true
					}
					if kind == "persistent_conflict" {
						return nil, APIError{409}, true
					}
				}
				return nil, nil, false
			}
			w := request(s, "PUT", "/notify-attach", "hook-test-credential", attachPayload(2))
			if w.Code != 503 && w.Code != 423 {
				t.Fatal("unsafe notification acknowledged")
			}
			puts := f.calls["PUT "+s.path("configmaps", s.config.NotificationsConfigMap)]
			if kind == "uncertain_put" && puts != 1 {
				t.Fatal("unknown write replayed")
			}
			if kind == "persistent_conflict" && puts != 6 {
				t.Fatal("conflict retry unbounded")
			}
			if kind == "capacity" && puts != 0 {
				t.Fatal("capacity guard mutated historical receipts")
			}
		})
	}
}
