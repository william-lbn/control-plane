package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	paths := []string{"capabilities", "memory", "initialization", "budget/shared", "work/state", "connections/state", "work/prepared", "work/dsm", "resize", "resize/status", "resize/cancel", "issuer/fence", "admission", "admission/status", "admission/lease", "admission/lease/rearm/status", "admission/close-original", "admission/close-original/status", "admission/continue-close", "admission/continue-close/status"}
	for _, path := range paths {
		method, body := http.MethodPost, []byte(`{"issuer_epoch":"18446744073709551615"}`)
		if path == "capabilities" || path == "memory" || path == "initialization" || path == "budget/shared" || path == "work/state" || path == "connections/state" || path == "work/prepared" || path == "work/dsm" {
			method, body = http.MethodGet, nil
		}
		code, data := proxyRequest(t, client, method, base+"/dsg/v1/"+path, token, body)
		if code != 200 || string(data) != `{"native_fact_fixture":true}` {
			t.Fatalf("route %s got %d", path, code)
		}
	}
	if calls.Load() != int32(len(paths)) {
		t.Fatal("not all native calls ran")
	}
}

func TestDSGAProxySuspensionDrainRequiresSettledExactClose(t *testing.T) {
	instance := dsgapi.Instance{VMUID: "vm", ComputeInstanceID: "compute", PGBootID: 91}
	close := dsgapi.AdmissionRequest{
		ProtocolVersion: 1, Instance: instance, IssuerEpoch: 3, Revision: 4,
		ExpectedCapacityGeneration: 5, BudgetCommitmentID: "budget",
	}
	state := journal.Model{Owner: journal.Owner{Epoch: 4}, PGInstance: &instance}
	body, err := json.Marshal(close)
	if err != nil {
		t.Fatal(err)
	}
	if validDSGASuspensionDrain(body, state) {
		t.Fatal("missing closure accepted")
	}
	state.ServiceClosure = &journal.ServiceClosure{Request: close, Phase: journal.Settled}
	if validDSGASuspensionDrain(body, state) {
		t.Fatal("non-suspension closure accepted")
	}
	state.ServiceClosure.SuspensionOperation = "op_suspension"
	state.ServiceClosure.Phase = journal.Unconfirmed
	if validDSGASuspensionDrain(body, state) {
		t.Fatal("unconfirmed closure accepted")
	}
	state.ServiceClosure.Phase = journal.Settled
	if !validDSGASuspensionDrain(body, state) {
		t.Fatal("settled close from predecessor rejected")
	}
	for _, changed := range []string{
		strings.Replace(string(body), `"revision":"4"`, `"revision":"5"`, 1),
		strings.Replace(string(body), `"pg_boot_id":"91"`, `"pg_boot_id":"92"`, 1),
		strings.Replace(string(body), `"open":false,`, "", 1),
		strings.Replace(string(body), `"open":false`, `"Open":false`, 1),
		strings.Replace(string(body), `"open":false`, `"open":true`, 1),
		strings.Replace(string(body), `"open":false`, `"open":false,"open":false`, 1),
	} {
		if changed == string(body) || validDSGASuspensionDrain([]byte(changed), state) {
			t.Fatal("invalid close accepted")
		}
	}
	state.ServiceClosure.Continuation = &journal.ServiceClosureContinuation{Phase: journal.Settled}
	state.ServiceClosure.Continuation.Request.Close = close
	state.ServiceClosure.Continuation.Request.Close.Revision++
	if validDSGASuspensionDrain(body, state) {
		t.Fatal("superseded close accepted")
	}
	body, err = json.Marshal(state.ServiceClosure.Continuation.Request.Close)
	if err != nil || !validDSGASuspensionDrain(body, state) {
		t.Fatal("settled successor close rejected")
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
	for _, path := range []string{"memory", "budget/shared", "work/state", "connections/state", "work/prepared", "work/dsm", "resize/status", "resize/cancel", "admission", "admission/status", "admission/lease", "admission/lease/rearm/status", "admission/close-original", "admission/close-original/status", "admission/continue-close", "admission/continue-close/status"} {
		method, body := http.MethodPost, []byte(`{}`)
		if path == "memory" || path == "budget/shared" || path == "work/state" || path == "connections/state" || path == "work/prepared" || path == "work/dsm" {
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

// 只证明网关显式转发与原严格有类型合同；native响应为单元夹具，不冒PG执行事实。
type admissionObserverOnlyTransport struct{}

func (admissionObserverOnlyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, net.InvalidAddrError("admission must use the independent observer pool")
}

func TestDSGAProxyAdmissionLeaseAndContinuationRoutes(t *testing.T) {
	lease := `{"protocol_version":1,"instance":{"vm_uid":"vm-uid","compute_instance_id":"compute-1","pg_boot_id":"7"},"issuer_epoch":"1","expected_capacity_generation":"3","expected_admission_revision":"2","expected_deadline_ns":"1234","action":"renew"}`
	continuation := `{"protocol_version":2,"original":{"protocol_version":1,"instance":{"vm_uid":"vm-uid","compute_instance_id":"compute-1","pg_boot_id":"7"},"issuer_epoch":"1","revision":"1","expected_capacity_generation":"1","open":false,"budget_commitment_id":"original-admission-budget"},"previous_close":{"protocol_version":1,"instance":{"vm_uid":"vm-uid","compute_instance_id":"compute-1","pg_boot_id":"7"},"issuer_epoch":"1","revision":"1","expected_capacity_generation":"1","open":false,"budget_commitment_id":"original-admission-budget"},"close":{"protocol_version":1,"instance":{"vm_uid":"vm-uid","compute_instance_id":"compute-1","pg_boot_id":"7"},"issuer_epoch":"1","revision":"2","expected_capacity_generation":"2","open":false,"budget_commitment_id":"original-admission-budget"}}`
	for _, scenario := range []struct {
		name, path, body string
		want             int
	}{
		{"observe", "/dsg/v1/admission/lease", strings.Replace(strings.Replace(lease, `"renew"`, `"observe"`, 1), `"1234"`, `"0"`, 1), 200},
		{"renew", "/dsg/v1/admission/lease", lease, 200},
		{"rearm", "/dsg/v1/admission/lease", strings.Replace(lease, `"renew"`, `"rearm"`, 1), 200},
		{"rearm_status", "/dsg/v1/admission/lease/rearm/status", strings.Replace(lease, `"renew"`, `"rearm"`, 1), 200},
		{"continue_close", "/dsg/v1/admission/continue-close", continuation, 200},
		{"continue_close_status", "/dsg/v1/admission/continue-close/status", continuation, 200},
		{"native_unknown_member", "/dsg/v1/admission/lease", strings.TrimSuffix(lease, "}") + `,"unknown":true}`, 400},
		{"native_zero_pg_boot", "/dsg/v1/admission/lease", strings.Replace(lease, `"pg_boot_id":"7"`, `"pg_boot_id":"0"`, 1), 400},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 保留空白和所有规范u64字符串；网关不重编码、更换epoch或制造deadline。
			body := []byte(" \n" + scenario.body + "\n")
			var calls atomic.Int32
			f, client, base, token := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				actual, _ := io.ReadAll(r.Body)
				if r.Method != http.MethodPost || r.URL.Path != scenario.path || !bytes.Equal(actual, body) || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-Forwarded-Host") != "" {
					t.Error("original admission route, bytes or credential changed")
				}
				w.Header().Set("Content-Type", "application/json")
				valid := false
				if strings.HasPrefix(scenario.path, "/dsg/v1/admission/lease") {
					var request dsgapi.AdmissionLeaseRequest
					valid = json.Unmarshal(actual, &request) == nil && request.Instance == (dsgapi.Instance{VMUID: "vm-uid", ComputeInstanceID: "compute-1", PGBootID: 7}) && request.IssuerEpoch == 1
					if scenario.path == "/dsg/v1/admission/lease/rearm/status" {
						valid = valid && request.Action == "rearm"
					}
				} else {
					var request dsgapi.AdmissionContinuationRequest
					valid = json.Unmarshal(actual, &request) == nil && request.Close.Instance == (dsgapi.Instance{VMUID: "vm-uid", ComputeInstanceID: "compute-1", PGBootID: 7}) && request.Close.IssuerEpoch == 1
				}
				if !valid {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
					return
				}
				_, _ = w.Write([]byte(`{"native_admission_fixture":true}`))
			})
			f.proxy.execution.Transport = admissionObserverOnlyTransport{}
			code, data := proxyRequest(t, client, http.MethodPost, base+scenario.path, token, body)
			if code != scenario.want || calls.Load() != 1 {
				t.Fatalf("admission route got HTTP %d, native calls %d", code, calls.Load())
			}
			want := `{"native_admission_fixture":true}`
			if scenario.want == 400 {
				want = `{"error":"invalid_request"}`
			}
			if string(data) != want {
				t.Fatal("native fact/status changed")
			}
		})
	}
}

func TestDSGAProxyAdmissionRoutesRetainOuterGates(t *testing.T) {
	for _, scenario := range []string{"wrong_method", "near_path", "query", "over_limit", "foreign_token", "owner_changed", "config_UID"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			f, client, base, token := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			})
			method, path, body := http.MethodPost, "/dsg/v1/admission/lease", []byte(`{}`)
			switch scenario {
			case "wrong_method":
				method = http.MethodGet
				body = nil
			case "near_path":
				path += "/renew"
			case "query":
				path += "?action=renew"
			case "over_limit":
				body = []byte(strings.Repeat("x", 16*1024+1))
			case "foreign_token":
				token = "invalid"
			case "owner_changed":
				f.mu.Lock()
				holder := "dsga-" + strings.Repeat("c", 32) + ".replacement"
				f.lease.Spec.HolderIdentity = &holder
				f.mu.Unlock()
			case "config_UID":
				f.mu.Lock()
				f.vm.Spec.DSGABootstrap.ConfigSecretUID = "recreated-config"
				f.mu.Unlock()
			}
			code, _ := proxyRequest(t, client, method, base+path, token, body)
			if code < 400 || calls.Load() != 0 {
				t.Fatalf("unsafe admission %s reached native (HTTP %d)", scenario, code)
			}
		})
	}
}

// 通过原TLS生产mux先实际签发array grant，再走同一proxy；只有最后Native处理器为夹具。
func proxyArrayGrant(t *testing.T, f *brokerTestAPI, client *http.Client, base, path string, body []byte) string {
	t.Helper()
	f.mu.Lock()
	claim := owner.Ownership{Namespace: f.vm.Namespace, VMName: f.vm.Name, VMUID: f.vm.UID,
		Incarnation: f.vm.Spec.DSGABootstrap.Incarnation, LeaseUID: f.lease.UID,
		HolderID: *f.lease.Spec.HolderIdentity, IssuerEpoch: 1}
	f.mu.Unlock()
	digest := sha256.Sum256(body)
	envelope := dsgaArrayCredentialEnvelope{ProtocolVersion: 1, Owner: claim,
		Request: dsgaArrayRequestBinding{1, http.MethodPost, path, hex.EncodeToString(digest[:])}}
	raw, _ := json.Marshal(envelope)
	code, data := brokerRequest(t, client, base+"/computes/"+brokerTestWorkload+"/dsga-array-credentials", raw)
	var grant struct {
		AccessToken string `json:"access_token"`
	}
	if code != http.StatusOK || json.Unmarshal(data, &grant) != nil || grant.AccessToken == "" {
		t.Fatalf("actual TLS array credential grant failed: HTTP %d", code)
	}
	return grant.AccessToken
}

func TestDSGAProxyArrayAuthorityRoutesPreserveOriginalBytesAndGrant(t *testing.T) {
	for _, path := range []string{"/dsg/v1/resource/array-authorize", "/dsg/v1/resource/array-observe", "/dsg/v1/resource/array-replay-original"} {
		t.Run(strings.TrimPrefix(path, "/dsg/v1/resource/"), func(t *testing.T) {
			var calls atomic.Int32
			body := []byte("{\n \"protocol_version\":1, \"opaque\":\"原始字节\" }\n")
			var token string
			f, client, base, _ := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				got, _ := io.ReadAll(r.Body)
				if r.Method != http.MethodPost || r.URL.Path != path || !bytes.Equal(got, body) || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-Forwarded-Host") != "" {
					t.Error("array proxy changed original request or leaked caller headers")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"native_array_fixture":true}`))
			})
			token = proxyArrayGrant(t, f, client, base, path, body)
			code, data := proxyRequest(t, client, http.MethodPost, base+path, token, body)
			if code != http.StatusOK || calls.Load() != 1 || string(data) != `{"native_array_fixture":true}` {
				t.Fatalf("array original route failed: HTTP %d, Native calls %d", code, calls.Load())
			}
		})
	}
}

func TestDSGAProxyArrayAuthorityRejectsScopeAndBodyConfusion(t *testing.T) {
	for _, scenario := range []string{"generic_array", "array_generic", "different_array_path", "body_byte_changed", "body_whitespace_changed", "wrong_method", "query_alias", "near_path"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			f, client, base, generic := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) })
			path, body := "/dsg/v1/resource/array-authorize", []byte(`{"protocol_version":1}`)
			token := proxyArrayGrant(t, f, client, base, path, body)
			method := http.MethodPost
			switch scenario {
			case "generic_array":
				token = generic
			case "array_generic":
				path, method, body = "/dsg/v1/memory", http.MethodGet, nil
			case "different_array_path":
				path = "/dsg/v1/resource/array-replay-original"
			case "body_byte_changed":
				body = []byte(`{"protocol_version":2}`)
			case "body_whitespace_changed":
				body = []byte(`{ "protocol_version":1}`)
			case "wrong_method":
				method, body = http.MethodGet, nil
			case "query_alias":
				path += "?mode=authorize"
			case "near_path":
				path += "/extra"
			}
			code, _ := proxyRequest(t, client, method, base+path, token, body)
			if code < 400 || calls.Load() != 0 {
				t.Fatalf("unsafe array %s reached Native (HTTP %d)", scenario, code)
			}
		})
	}
}

func TestDSGAProxyArrayAuthorityRetainsOwnerConfigServiceGates(t *testing.T) {
	for _, scenario := range []string{"holder_changed", "lease_expired", "lease_future", "config_UID", "service_selector", "service_port", "service_deleting"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			f, client, base, _ := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) })
			path, body := "/dsg/v1/resource/array-authorize", []byte(`{"protocol_version":1}`)
			token := proxyArrayGrant(t, f, client, base, path, body)
			f.mu.Lock()
			switch scenario {
			case "holder_changed":
				holder := "dsga-" + strings.Repeat("c", 32) + ".replacement"
				f.lease.Spec.HolderIdentity = &holder
			case "lease_expired":
				old := metav1.NewMicroTime(time.Now().Add(-time.Minute))
				f.lease.Spec.RenewTime = &old
			case "lease_future":
				future := metav1.NewMicroTime(time.Now().Add(time.Minute))
				f.lease.Spec.RenewTime = &future
			case "config_UID":
				f.vm.Spec.DSGABootstrap.ConfigSecretUID = "recreated-config"
			case "service_selector":
				f.service.Spec.Selector["vm.neon.tech/name"] = "cp-ffffffffffffffff"
			case "service_port":
				f.service.Spec.Ports[0].TargetPort = intstr.FromInt(9999)
			case "service_deleting":
				now := metav1.Now()
				f.service.DeletionTimestamp = &now
			}
			f.mu.Unlock()
			code, _ := proxyRequest(t, client, http.MethodPost, base+path, token, body)
			if code < 400 || calls.Load() != 0 {
				t.Fatalf("unsafe source %s reached Native (HTTP %d)", scenario, code)
			}
		})
	}
}

func TestDSGAProxyArrayAuthorityStrictSignedRequestClaims(t *testing.T) {
	for _, scenario := range []string{"missing_request", "null_request", "unknown_request", "request_alias", "request_duplicate", "method_alias", "generic_request", "generic_null_request", "unknown_member", "wrong_protocol", "bad_digest"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			f, client, base, _ := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) })
			path, body := "/dsg/v1/resource/array-authorize", []byte(`{"protocol_version":1}`)
			token := proxyArrayGrant(t, f, client, base, path, body)
			parts := strings.Split(token, ".")
			raw, err := base64.RawURLEncoding.DecodeString(parts[1])
			if err != nil {
				t.Fatal("fixture token decode")
			}
			var claims map[string]json.RawMessage
			if json.Unmarshal(raw, &claims) != nil {
				t.Fatal("fixture token claims")
			}
			switch scenario {
			case "missing_request":
				delete(claims, "request")
			case "null_request":
				claims["request"] = json.RawMessage("null")
			case "unknown_request":
				claims["request"] = json.RawMessage(strings.TrimSuffix(string(claims["request"]), "}") + `,"extra":true}`)
			case "request_alias":
				claims["Request"] = claims["request"]
				delete(claims, "request")
			case "request_duplicate":
				claims["request"] = json.RawMessage(strings.Replace(string(claims["request"]), `"protocol_version":1`, `"protocol_version":1,"protocol_version":1`, 1))
			case "method_alias":
				claims["request"] = json.RawMessage(strings.Replace(string(claims["request"]), `"method":`, `"Method":`, 1))
			case "generic_request":
				claims["scope"] = json.RawMessage(`"compute_ctl:dsga"`)
			case "generic_null_request":
				claims["scope"] = json.RawMessage(`"compute_ctl:dsga"`)
				claims["request"] = json.RawMessage("null")
			case "unknown_member":
				claims["unknown"] = json.RawMessage("true")
			case "wrong_protocol":
				claims["request"] = json.RawMessage(strings.Replace(string(claims["request"]), `"protocol_version":1`, `"protocol_version":2`, 1))
			case "bad_digest":
				claims["request"] = json.RawMessage(strings.Replace(string(claims["request"]), `"body_sha256":"`, `"body_sha256":"z`, 1))
			}
			raw, _ = json.Marshal(claims)
			f.mu.Lock()
			seed := append([]byte(nil), f.credentials.Data["controlSigningSeed"]...)
			f.mu.Unlock()
			token = proxySigned(ed25519.NewKeyFromSeed(seed), raw)
			code, _ := proxyRequest(t, client, http.MethodPost, base+path, token, body)
			if code < 400 || calls.Load() != 0 {
				t.Fatalf("ambiguous signed %s reached Native (HTTP %d)", scenario, code)
			}
		})
	}
}

func TestDSGAProxyArrayObserveIndependentFromBlockedAuthority(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var observed atomic.Int32
	f, client, base, _ := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dsg/v1/resource/array-authorize" {
			close(entered)
			<-release
		} else {
			observed.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"fixture":true}`))
	})
	f.proxy.execution.Transport.(*http.Transport).MaxConnsPerHost = 1
	body := []byte(`{"protocol_version":1}`)
	execToken := proxyArrayGrant(t, f, client, base, "/dsg/v1/resource/array-authorize", body)
	observeToken := proxyArrayGrant(t, f, client, base, "/dsg/v1/resource/array-observe", body)
	done := make(chan struct{})
	go func() {
		defer close(done)
		request, _ := http.NewRequest(http.MethodPost, base+"/dsg/v1/resource/array-authorize", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+execToken)
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("array authority did not reach Native")
	}
	started := time.Now()
	code, _ := proxyRequest(t, client, http.MethodPost, base+"/dsg/v1/resource/array-observe", observeToken, body)
	if code != http.StatusOK || observed.Load() != 1 || time.Since(started) > 2*time.Second {
		t.Fatal("array observation blocked behind authority execution")
	}
	// 等待执行在handler退出时结束；release由defer关闭，连接池仍由原fixture清理。
	_ = done
}
