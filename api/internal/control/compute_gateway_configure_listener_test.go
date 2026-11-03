package control

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type configureListenerTransport func(*http.Request) (*http.Response, error)

func (f configureListenerTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// 只用单元fixture验证正式routes派发和原configure鉴权，不把mock算作真实K8s/TLS。
func configureListenerUnit(t *testing.T) (*http.ServeMux, ed25519.PrivateKey, map[string]any, *atomic.Int64) {
	t.Helper()
	_, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	config := map[string]any{"spec": map[string]any{"mode": "Primary", "tenant_id": "unit-tenant", "timeline_id": "unit-timeline", "cluster": map[string]any{"cluster_id": "unit-project", "settings": []any{map[string]any{"name": "neon.tenant_id", "value": "unit-tenant"}, map[string]any{"name": "neon.timeline_id", "value": "unit-timeline"}, map[string]any{"name": "neon.pageserver_connstring", "value": "unit-pageserver:6400"}}}}}
	installComputeJWK(config, key)
	raw, _ := json.Marshal(config)
	endpoint := "ep_0123456789abcdef"
	calls := &atomic.Int64{}
	kube := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/secrets/"):
			_ = json.NewEncoder(w).Encode(record{"metadata": record{"labels": record{"neon-control/project-id": "unit-project", "neon-control/endpoint-id": endpoint}}, "data": record{"config.json": base64.StdEncoding.EncodeToString(raw)}})
		case strings.Contains(r.URL.Path, "/services/"):
			_ = json.NewEncoder(w).Encode(record{"metadata": record{"labels": record{"neon-control/project-id": "unit-project", "neon-control/endpoint-id": endpoint}}, "spec": record{"clusterIP": "127.0.0.1"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(kube.Close)
	gateway := &computeGateway{kube: &kubeClient{base: kube.URL, namespace: "neon", http: kube.Client()}, http: &http.Client{Transport: configureListenerTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Host != "127.0.0.1:3080" || r.URL.Path != "/configure" {
			t.Error("original fixed target changed")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"status":"configuration_received"}`))}, nil
	})}}
	mux, e := gateway.routes("kube-system", "autoscaler-agent")
	if e != nil {
		t.Fatal(e)
	}
	return mux, key, config, calls
}
func configureListenerRequest(mux *http.ServeMux, token, sni string, config map[string]any) *httptest.ResponseRecorder {
	body, _ := json.Marshal(config)
	r := httptest.NewRequest("POST", "https://unit/"+"computes/cp-0123456789abcdef/configure", bytes.NewReader(body))
	r.TLS = &tls.ConnectionState{ServerName: sni}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}
func TestGatewayDedicatedConfigureAcceptsOriginalComputeJWT(t *testing.T) {
	mux, key, config, calls := configureListenerUnit(t)
	w := configureListenerRequest(mux, computeJWT(key, "ep_0123456789abcdef", time.Now()), "cp-0123456789abcdef-management.neon.svc", config)
	if w.Code != 200 || calls.Load() != 1 {
		t.Fatal("original compute JWT did not reach original configure handler")
	}
}
func TestGatewayDedicatedConfigureRejectsServiceAccountAndScopedJWT(t *testing.T) {
	mux, key, config, calls := configureListenerUnit(t)
	now := time.Now()
	header, _ := json.Marshal(record{"alg": "EdDSA", "kid": computeKeyID(key)})
	claims, _ := json.Marshal(record{"compute_id": "cp-0123456789abcdef", "scope": "dsga-resource", "aud": []string{"compute"}, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()})
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	scoped := input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(input)))
	for name, token := range map[string]string{"service-account": "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJzeXN0ZW06c2VydmljZWFjY291bnQ6a3ViZS1zeXN0ZW06YXV0b3NjYWxlci1hZ2VudCJ9.unit", "scoped-DSGA": scoped} {
		t.Run(name, func(t *testing.T) {
			w := configureListenerRequest(mux, token, "cp-0123456789abcdef-management.neon.svc", config)
			if w.Code != 401 {
				t.Fatal("non-compute credential crossed configure gate")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("rejected credential reached backend")
	}
}
func TestGatewayDedicatedConfigureKeepsOriginalSNIAndSpecIdentityGates(t *testing.T) {
	mux, key, config, calls := configureListenerUnit(t)
	token := computeJWT(key, "ep_0123456789abcdef", time.Now())
	t.Run("wrong-SNI", func(t *testing.T) {
		w := configureListenerRequest(mux, token, "cp-fedcba9876543210-management.neon.svc", config)
		if w.Code != 403 {
			t.Fatal("wrong SNI accepted")
		}
	})
	t.Run("wrong-tenant", func(t *testing.T) {
		raw, _ := json.Marshal(config)
		var changed map[string]any
		_ = json.Unmarshal(raw, &changed)
		changed["spec"].(map[string]any)["tenant_id"] = "foreign-unit-tenant"
		w := configureListenerRequest(mux, token, "cp-0123456789abcdef-management.neon.svc", changed)
		if w.Code != 403 {
			t.Fatal("foreign spec identity accepted")
		}
	})
	if calls.Load() != 0 {
		t.Fatal("rejected identity reached backend")
	}
}
