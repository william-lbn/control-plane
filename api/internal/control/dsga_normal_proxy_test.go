package control

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"
)

// 仅新增normal四固定路由：真实TLS/TokenReview/短令牌/原Proxy入口，内部
// Compute peer仍是协议夹具。该U不代表PG、Kubernetes或normal预算通过。
func TestDSGANormalProxyFixedRoutesPreserveBytes(t *testing.T) {
	var calls atomic.Int32
	body := []byte(`{"original_envelope":"unchanged","upper":"18446744073709551615"}`)
	_, client, base, token := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, e := io.ReadAll(r.Body)
		if e != nil || string(b) != string(body) || r.Method != http.MethodPost || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-Forwarded-Host") != "" {
			t.Error("normal original packet changed")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"original_root_fact":"unchanged"}`))
	})
	for _, path := range []string{"normal-apply", "normal-observe", "normal-replay-original", "normal-recover"} {
		t.Run(path, func(t *testing.T) {
			address := base + "/dsg/v1/resource/" + path
			code, b := proxyRequest(t, client, http.MethodPost, address, token, body)
			if code != 200 || string(b) != `{"original_root_fact":"unchanged"}` {
				t.Fatalf("normal fixed route rejected: %d", code)
			}
			before := calls.Load()
			code, _ = proxyRequest(t, client, http.MethodGet, address, token, nil)
			if code < 400 || calls.Load() != before {
				t.Fatal("normal route accepted wrong method")
			}
		})
	}
	if calls.Load() != 4 {
		t.Fatal("normal original targets were not exact")
	}
	before := calls.Load()
	code, _ := proxyRequest(t, client, http.MethodPost, base+"/dsg/v1/resource/normal-unknown", token, body)
	if code < 400 || calls.Load() != before {
		t.Fatal("unknown normal path reached native")
	}
}
func TestDSGANormalProxyKeepsOriginalOwnerAndServiceGates(t *testing.T) {
	for _, name := range []string{"owner", "service"} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			f, client, base, token := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			})
			f.mu.Lock()
			if name == "owner" {
				holder := "dsga-cccccccccccccccccccccccccccccccc.replacement"
				f.lease.Spec.HolderIdentity = &holder
			} else {
				f.service.Spec.Selector["vm.neon.tech/name"] = "replacement"
			}
			f.mu.Unlock()
			code, _ := proxyRequest(t, client, http.MethodPost, base+"/dsg/v1/resource/normal-apply", token, []byte(`{}`))
			if code < 400 || calls.Load() != 0 {
				t.Fatalf("normal path bypassed original %s gate", name)
			}
		})
	}
}
