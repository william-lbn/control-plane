package control

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"
)

// 只验证本批新增固定资源路由，复用实际TLS网关夹具；不重复原PG路由全套，
// 不把客户端/Lease API夹具称作真实K8s或root执行器验收。
func TestDSGAProxyResourceRoutesPreserveOriginalAndRejectAliases(t *testing.T) {
	var calls atomic.Int32
	_, client, base, token := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost && string(body) != `{"issuer_epoch":"9007199254740993"}` {
			t.Error("resource original bytes changed")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"fixture":true}`))
	})
	for _, path := range []string{"identity", "fence", "apply", "observe", "replay-original"} {
		method, body := http.MethodPost, []byte(`{"issuer_epoch":"9007199254740993"}`)
		if path == "identity" {
			method, body = http.MethodGet, nil
		}
		code, data := proxyRequest(t, client, method, base+"/dsg/v1/resource/"+path, token, body)
		if code != 200 || string(data) != `{"fixture":true}` {
			t.Fatalf("resource %s: %d", path, code)
		}
	}
	for _, path := range []string{"initialize", "bootstrap", "apply?override=1", "%61pply"} {
		code, _ := proxyRequest(t, client, http.MethodPost, base+"/dsg/v1/resource/"+path, token, []byte(`{}`))
		if code == 200 {
			t.Fatal("unsafe resource path accepted", path)
		}
	}
	if calls.Load() != 5 {
		t.Fatal("unexpected resource native calls", calls.Load())
	}
}
