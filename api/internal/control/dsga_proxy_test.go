package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/compute"
	"github.com/neondatabase/autoscaling/pkg/dsga/credential"
	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	"github.com/neondatabase/autoscaling/pkg/dsga/owner"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func proxySigned(key ed25519.PrivateKey, raw []byte) string {
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": computeKeyID(key)})
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(raw)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(input)))
}

func TestDSGAProxyJWTMatchesStrictNativeContract(t *testing.T) {
	f, _, _, key, _, _ := newBrokerTest(t)
	var config dsgaCredentialConfig
	if json.Unmarshal(f.config.Data["config.json"], &config) != nil {
		t.Fatal("bad test config")
	}
	now := time.Now()
	claims := func() map[string]any {
		return map[string]any{"aud": "neon-dsga", "scope": "compute_ctl:dsga", "compute_id": brokerTestWorkload, "vm_uid": "vm-uid", "compute_instance_id": "compute-1", "issuer_epoch": "1", "nbf": now.Unix() - 2, "exp": now.Unix() + 60}
	}
	encode := func(value map[string]any) string { raw, _ := json.Marshal(value); return proxySigned(key, raw) }
	if _, valid := validDSGAProxyJWT(encode(claims()), brokerTestWorkload, config, now); !valid {
		t.Fatal("valid dedicated token rejected")
	}
	for field, value := range map[string]any{"aud": []string{"neon-dsga"}, "scope": "admin", "compute_id": "cp-ffffffffffffffff", "vm_uid": "recreated", "compute_instance_id": "replacement", "issuer_epoch": 1, "nbf": now.Unix() + 1, "exp": now.Unix(), "unknown": true} {
		t.Run(field, func(t *testing.T) {
			c := claims()
			c[field] = value
			if _, valid := validDSGAProxyJWT(encode(c), brokerTestWorkload, config, now); valid {
				t.Fatal("unsafe signed claims accepted")
			}
		})
	}
	validRaw, _ := json.Marshal(claims())
	for name, raw := range map[string][]byte{
		"duplicate_member": []byte(strings.Replace(string(validRaw), `"aud":"neon-dsga"`, `"aud":"neon-dsga","aud":"neon-dsga"`, 1)),
		"negative_time":    []byte(`{"aud":"neon-dsga","scope":"compute_ctl:dsga","compute_id":"` + brokerTestWorkload + `","vm_uid":"vm-uid","compute_instance_id":"compute-1","issuer_epoch":"1","nbf":-1,"exp":2}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, valid := validDSGAProxyJWT(proxySigned(key, raw), brokerTestWorkload, config, now); valid {
				t.Fatal("ambiguous signed claims accepted")
			}
		})
	}
	tampered := strings.Split(encode(claims()), ".")
	signature, _ := base64.RawURLEncoding.DecodeString(tampered[2])
	signature[0] ^= 1
	tampered[2] = base64.RawURLEncoding.EncodeToString(signature)
	for name, token := range map[string]string{"general": computeJWT(key, "ep_0123456789abcdef", now), "bad_signature": strings.Join(tampered, "."), "bad_segments": "x.y.z"} {
		t.Run(name, func(t *testing.T) {
			if _, valid := validDSGAProxyJWT(token, brokerTestWorkload, config, now); valid {
				t.Fatal("foreign or malformed JWT accepted")
			}
		})
	}
	t.Run("unbounded_lifetime", func(t *testing.T) {
		c := claims()
		c["exp"] = now.Unix() + 300
		if _, valid := validDSGAProxyJWT(encode(c), brokerTestWorkload, config, now); valid {
			t.Fatal("lifetime includes nbf and must be at most 300")
		}
	})
	t.Run("duplicate_jwk", func(t *testing.T) {
		config.Ctl.JWKS.Keys = append(config.Ctl.JWKS.Keys, config.Ctl.JWKS.Keys[0])
		if _, valid := validDSGAProxyJWT(encode(claims()), brokerTestWorkload, config, now); valid {
			t.Fatal("ambiguous key id accepted")
		}
	})
}

// 从实际TLS凭据路由取签名，再走同一实际生产mux/Proxy/typed API与本机HTTP。
// 只有内部native最终处理器是协议夹具，不能把此测试称为真实PG或K8s认证。
func proxyFixture(t *testing.T, native http.HandlerFunc) (*brokerTestAPI, *http.Client, string, string) {
	t.Helper()
	f, _, claim, _, client, address := newBrokerTest(t)
	server := httptest.NewServer(native)
	t.Cleanup(server.Close)
	f.service = &core.Service{ObjectMeta: metav1.ObjectMeta{Name: brokerTestWorkload + "-management", Namespace: "neon", UID: "service-uid", ResourceVersion: "1", Labels: credentialLabels("project-1", "ep_0123456789abcdef")},
		Spec: core.ServiceSpec{Type: core.ServiceTypeClusterIP, ClusterIP: "10.0.0.7", Selector: map[string]string{"vm.neon.tech/name": brokerTestWorkload}, Ports: []core.ServicePort{{Name: "compute-control", Port: 3080, Protocol: core.ProtocolTCP, TargetPort: intstr.FromInt(3080)}}}}
	for _, pool := range []*http.Client{f.proxy.execution, f.proxy.observer} {
		transport := pool.Transport.(*http.Transport)
		transport.DialContext = func(ctx context.Context, network, target string) (net.Conn, error) {
			if network != "tcp" || target != "10.0.0.7:3080" {
				t.Error("proxy accepted caller-provided target")
				return nil, net.InvalidAddrError("unexpected target")
			}
			return new(net.Dialer).DialContext(ctx, network, server.Listener.Addr().String())
		}
		t.Cleanup(transport.CloseIdleConnections)
	}
	body, _ := json.Marshal(claim)
	code, data := brokerRequest(t, client, address, body)
	var grant struct {
		AccessToken string `json:"access_token"`
	}
	if code != 200 || json.Unmarshal(data, &grant) != nil || grant.AccessToken == "" {
		t.Fatal("actual broker did not issue a dedicated token")
	}
	return f, client, strings.TrimSuffix(address, "/computes/"+brokerTestWorkload+"/dsga-credentials"), grant.AccessToken
}

func proxyRequest(t *testing.T, client *http.Client, method, address, token string, body []byte) (int, []byte) {
	t.Helper()
	r, err := http.NewRequest(method, address, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	// 恶意/无关头只能留在入站，不能诱使内部重放、换目标或转发身份。
	r.Header.Set("Idempotency-Key", "must-not-forward")
	r.Header.Set("X-Forwarded-Host", "foreign.invalid")
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Set-Cookie") != "" || response.Header.Get("Location") != "" {
		t.Fatal("unsafe response headers")
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

func TestDSGAProxyExplicitRoutesPreserveOriginalBytesAndCredential(t *testing.T) {
	var calls atomic.Int32
	_, client, base, token := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-Forwarded-Host") != "" || r.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			t.Error("unsafe forwarded headers")
		}
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost && string(body) != `{"issuer_epoch":"18446744073709551615"}` {
			t.Error("body bytes changed")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "not-forwarded")
		w.Header().Set("Location", "https://foreign.invalid")
		_, _ = w.Write([]byte(`{"native_fact_fixture":true}`))
	})
	for _, path := range []string{"capabilities", "memory", "initialization", "resize", "resize/status", "resize/cancel", "issuer/fence", "admission", "admission/status", "admission/close-original", "admission/close-original/status"} {
		method, body := http.MethodPost, []byte(`{"issuer_epoch":"18446744073709551615"}`)
		if path == "capabilities" || path == "memory" || path == "initialization" {
			method, body = http.MethodGet, nil
		}
		code, data := proxyRequest(t, client, method, base+"/dsg/v1/"+path, token, body)
		if code != 200 || string(data) != `{"native_fact_fixture":true}` {
			t.Fatalf("route %s got %d", path, code)
		}
	}
	if calls.Load() != 11 {
		t.Fatal("not all native calls ran")
	}
}

func TestDSGAProxyRejectsStaleOwnerAndUnsafeServiceBeforeNative(t *testing.T) {
	for _, scenario := range []string{"owner_epoch", "lease_expired", "vm_recreated", "unmanaged", "service_project", "service_selector", "service_external", "service_port", "bad_journal", "bootstrap_config", "config_owner"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			f, client, base, token := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			})
			f.mu.Lock()
			switch scenario {
			case "bootstrap_config":
				f.vm.Spec.DSGABootstrap.ConfigSecretUID = "recreated-config"
			case "config_owner":
				f.config.OwnerReferences[0].UID = "foreign-vm"
			case "owner_epoch":
				holder := "dsga-" + strings.Repeat("c", 32) + ".replacement"
				f.lease.Spec.HolderIdentity = &holder
			case "lease_expired":
				old := metav1.NewMicroTime(time.Now().Add(-time.Hour))
				f.lease.Spec.RenewTime = &old
			case "vm_recreated":
				f.vm.UID = "recreated-vm"
			case "unmanaged":
				f.vm.Annotations = nil
			case "bad_journal":
				for key := range f.vm.Annotations {
					if strings.Contains(key, "journal") {
						f.vm.Annotations[key] = "malformed"
					}
				}
			case "service_project":
				f.service.Labels["neon-control/project-id"] = "foreign"
			case "service_selector":
				f.service.Spec.Selector["vm.neon.tech/name"] = "foreign"
			case "service_external":
				f.service.Spec.Type = core.ServiceTypeExternalName
				f.service.Spec.ExternalName = "foreign.invalid"
			case "service_port":
				f.service.Spec.Ports[0].TargetPort = intstr.FromInt(9999)
			}
			f.mu.Unlock()
			code, _ := proxyRequest(t, client, http.MethodGet, base+"/dsg/v1/capabilities", token, nil)
			if code < 400 || calls.Load() != 0 {
				t.Fatalf("unsafe %s reached native (%d)", scenario, code)
			}
		})
	}
}

func TestDSGAProxyObservationIndependentFromBlockedExecution(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	f, client, base, token := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dsg/v1/resize" {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	if f.proxy.execution.Transport == f.proxy.observer.Transport {
		t.Fatal("execution and observation share a transport")
	}
	f.proxy.execution.Transport.(*http.Transport).MaxConnsPerHost = 1
	f.proxy.observer.Transport.(*http.Transport).MaxConnsPerHost = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/dsg/v1/resize", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		defer close(done)
		response, err := client.Do(r)
		if err == nil {
			response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("execution did not enter")
	}
	for _, path := range []string{"memory", "resize/status", "resize/cancel", "admission", "admission/status", "admission/close-original", "admission/close-original/status"} {
		method, body := http.MethodPost, []byte(`{}`)
		if path == "memory" {
			method, body = http.MethodGet, nil
		}
		code, _ := proxyRequest(t, client, method, base+"/dsg/v1/"+path, token, body)
		if code != 200 {
			t.Fatal("blocked observation/cancel", path, code)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request cancellation did not complete")
	}
}

func TestDSGACredentialClientOwnerSDKAndProductionProxyChain(t *testing.T) {
	var nativeCalls atomic.Int32
	f, _, base, _ := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		nativeCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		history, provider, boot := uint32(16), false, dsgapi.Uint64(501)
		_ = json.NewEncoder(w).Encode(dsgapi.Capabilities{ProtocolVersion: 1, ProductionReady: false, VMShrinkPermit: false, Supported: true, BufferAPIVersion: 6, Enabled: true, InstanceBound: true,
			HistoryCapacity: &history, ProviderCanRelease: &provider, Instance: &dsgapi.DiscoveryIdentity{VMUID: "vm-uid", ComputeInstanceID: "compute-1", PGBootID: &boot}})
	})
	// 让真实Elector通过已有Lease的UID/RV CAS接管；不是test Authority。
	f.mu.Lock()
	old := metav1.NewMicroTime(time.Now().Add(-time.Hour))
	f.lease.Spec.RenewTime = &old
	// client-go按本地观察经过的LeaseDuration判定可接管，不能仅凭远端旧
	// 时间戳立即抢占。缩短夹具旧Lease为1秒，真实等待后仍须UID/RV CAS。
	oldDuration := int32(1)
	f.lease.Spec.LeaseDurationSeconds = &oldDuration
	f.mu.Unlock()
	broker, err := newDSGACredentialBroker(f.kube, "agents", "autoscaler")
	if err != nil {
		t.Fatal(err)
	}
	// Store从本机typed API取完整日志，不构造压缩journal/owner镜像。
	store, err := journal.NewStore(broker.vms.NeonvmV1().VirtualMachines("neon"), journal.Binding{Namespace: "neon", Name: brokerTestWorkload, VMUID: "vm-uid", Incarnation: strings.Repeat("a", 32)})
	if err != nil {
		t.Fatal(err)
	}
	options := owner.DefaultOptions()
	options.PodUID = "pod-uid"
	options.LeaseDuration, options.RenewDeadline, options.RetryPeriod = 6*time.Second, 4*time.Second, 200*time.Millisecond
	elector, err := owner.New(broker.coord, store, options)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "projected-token")
	if os.WriteFile(file, []byte("projected-agent-token"), 0600) != nil {
		t.Fatal("test token write")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = elector.Run(ctx, func(ctx context.Context, session *owner.Session) error {
		if session.Actor().Epoch != 2 {
			t.Fatal("owner did not use persisted takeover epoch")
		}
		credentials, err := credential.New(session, credential.Config{BaseURL: base, CAFile: f.managementCAFile, TokenFile: file, Timeout: 3 * time.Second})
		if err != nil {
			return err
		}
		defer credentials.CloseIdleConnections()
		identity, err := credentials.Identity(ctx)
		if err != nil {
			return err
		}
		if identity.ComputeID != brokerTestWorkload || identity.IssuerEpoch != 2 {
			t.Fatal("wrong broker identity")
		}
		worker, err := session.NewPGWorker(ctx, compute.Config{BaseURL: base, Instance: dsgapi.Instance{VMUID: identity.VMUID, ComputeInstanceID: identity.ComputeInstanceID, PGBootID: 0}, IssuerEpoch: identity.IssuerEpoch, Timeout: 5 * time.Second, Transport: credentials.ManagementTransport(), Token: credentials.Token, Pending: nil})
		if err != nil {
			return err
		}
		defer worker.CloseIdleConnections()
		instance, epoch := worker.Binding()
		if instance.PGBootID != 501 || epoch != 2 {
			t.Fatal("SDK did not freeze original discovered boot")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if nativeCalls.Load() != 1 {
		t.Fatal("actual management route did not reach native fixture exactly once")
	}
}

func TestDSGAProxyNeverFollowsRedirectOrRetriesLostPOST(t *testing.T) {
	for _, behavior := range []string{"redirect", "lost_reply", "over_limit", "invalid_json"} {
		t.Run(behavior, func(t *testing.T) {
			var calls atomic.Int32
			_, client, base, token := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch behavior {
				case "redirect":
					w.Header().Set("Location", "https://foreign.invalid")
					w.WriteHeader(307)
				case "lost_reply":
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					connection.Close()
				case "over_limit":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`"` + strings.Repeat("x", 1<<20) + `"`))
				case "invalid_json":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"truncated":`))
				}
			})
			code, _ := proxyRequest(t, client, http.MethodPost, base+"/dsg/v1/resize", token, []byte(`{}`))
			if code != 502 || calls.Load() != 1 {
				t.Fatalf("unsafe result/retry %d/%d", code, calls.Load())
			}
		})
	}
}

func TestDSGAProxyRejectsInvalidEnvelopeBeforeNative(t *testing.T) {
	var calls atomic.Int32
	_, client, base, token := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	for _, scenario := range []string{"wrong_method", "unknown_path", "query_target", "empty_query", "escaped_path", "get_body", "duplicate_auth", "missing_auth", "duplicate_media", "compressed_body", "empty_post", "over_limit"} {
		t.Run(scenario, func(t *testing.T) {
			method, path, body := http.MethodPost, "/dsg/v1/resize", `{}`
			switch scenario {
			case "wrong_method":
				method = http.MethodDelete
			case "unknown_path":
				path = "/dsg/v1/unknown"
			case "query_target":
				path += "?target=http://foreign.invalid"
			case "empty_query":
				path += "?"
			case "escaped_path":
				method, path, body = http.MethodGet, "/dsg/v1/cap%61bilities", ""
			case "get_body":
				method, path = http.MethodGet, "/dsg/v1/memory"
			case "empty_post":
				body = ""
			case "over_limit":
				body = strings.Repeat("x", 16*1024+1)
			}
			request, err := http.NewRequest(method, base+path, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			switch scenario {
			case "duplicate_auth":
				request.Header.Add("Authorization", "Bearer "+token)
			case "missing_auth":
				request.Header.Del("Authorization")
			case "duplicate_media":
				request.Header.Add("Content-Type", "application/json")
			case "compressed_body":
				request.Header.Set("Content-Encoding", "gzip")
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode < 400 || calls.Load() != 0 || response.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("unsafe envelope reached native", response.StatusCode)
			}
		})
	}
}
