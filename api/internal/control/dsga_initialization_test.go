package control

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// 只作为HTTP合同样本，不把合成数组成本当成PG物理证据。真实Rust/PG初始化
// 在Neon原生门禁另外执行；这里验证实际typed来源/元数据库/网络消费与拒绝顺序。
func initializationFactFixture(t *testing.T, p createPayload, plan dsgaCreationPlan, vm string) dsgaInitializationResponse {
	t.Helper()
	startup, err := plan.nativeBootstrap(p, vm)
	if err != nil {
		t.Fatal(err)
	}
	fact := dsgaInitializationResponse{ProtocolVersion: 1, OperationUUID: plan.OperationUUID, TenantID: p.TenantID, TimelineID: p.TimelineID,
		Startup: startup, AuthenticationPolicyVerified: true, OrdinaryRolesVerified: true,
		Observed: dsgapi.Snapshot{Instance: dsgapi.Instance{VMUID: vm, ComputeInstanceID: plan.ComputeInstanceID, PGBootID: 17}, CapacityGeneration: 1,
			LowBuffers: uint32(plan.Budget.BootstrapBufferBytes / 8192), HighBuffers: uint32(plan.Budget.BootstrapBufferBytes / 8192), MaximumBuffers: uint32(plan.Budget.MaximumBufferBytes / 8192), StartupSharedUpperBytes: 128 << 20}}
	for _, kind := range []dsgapi.ArrayKind{dsgapi.Descriptors, dsgapi.Blocks, dsgapi.IOCV, dsgapi.Checkpoint} {
		fact.Observed.Arrays = append(fact.Observed.Arrays, dsgapi.ArrayFact{Array: kind, CommittedUpperBytes: 4096, ReleaseState: dsgapi.ReleaseNone, Provider: dsgapi.ProviderFact{Status: dsgapi.ProviderUnknown}})
	}
	if err := fact.validate(p, plan, vm); err != nil {
		t.Fatal("invalid protocol fixture")
	}
	return fact
}

type initializationTestState struct {
	mu        sync.Mutex
	raw       []byte
	encoding  string
	afterRead func()
	calls     atomic.Int32
}

func initializationBindingFixture(t *testing.T) (context.Context, *dsgaCreationBinding, *creationTestAPI, **core.Secret, *core.Service, *http.Client, *initializationTestState, dsgaInitializationResponse) {
	t.Helper()
	ctx, binding, services, f, source := dsgaServicesSourceFixture(t, "success")
	if err := binding.prepareServices(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	vm := f.vm.DeepCopy()
	f.mu.Unlock()
	plan, err := binding.creationPlan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.vm.Status.Phase = vmv1.VmRunning
	key := ed25519.NewKeyFromSeed((*source).Data["controlSigningSeed"])
	f.mu.Unlock()
	services.mu.Lock()
	service := services.objects[vm.Name+"-management"].DeepCopy()
	services.mu.Unlock()
	nativeTarget := net.JoinHostPort(service.Spec.ClusterIP, "3080")
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer creation-kube-identity" {
			creationStatus(w, 403)
			return
		}
		if r.Method == http.MethodGet && (r.URL.Path == f.kube.path("secret", vm.Name+"-credentials") || r.URL.Path == f.kube.path("service", vm.Name+"-management")) {
			f.mu.Lock()
			defer f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if strings.Contains(r.URL.Path, "/services/") {
				_ = json.NewEncoder(w).Encode(service)
			} else {
				_ = json.NewEncoder(w).Encode(*source)
			}
			return
		}
		f.serve(w, r)
	}))
	t.Cleanup(api.Close)
	f.kube.base, f.kube.http = api.URL, api.Client()
	fact := initializationFactFixture(t, binding.intent.createPayload, plan, string(vm.UID))
	state := &initializationTestState{}
	state.raw, _ = json.Marshal(fact)
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/dsg/v1/initialization" || r.URL.RawQuery != "" {
			t.Error("unexpected native request")
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			t.Error("missing native read credential")
			w.WriteHeader(401)
			return
		}
		signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
		raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims dsgaProxyClaims
		if !ed25519.Verify(key.Public().(ed25519.PublicKey), []byte(parts[0]+"."+parts[1]), signature) || dsgaTokenObject(raw, &claims) != nil ||
			claims.Scope != "compute_ctl:dsga_initialization" || claims.VMUID != string(vm.UID) || claims.ComputeInstanceID != plan.ComputeInstanceID || claims.Expires <= uint64(time.Now().Unix()) {
			t.Error("native request used foreign/broad credential")
		}
		state.mu.Lock()
		body, encoding, hook := append([]byte(nil), state.raw...), state.encoding, state.afterRead
		state.mu.Unlock()
		if hook != nil {
			hook()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if encoding != "" {
			w.Header().Set("Content-Encoding", encoding)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(native.Close)
	client := newDSGAProxy(nil).observer
	transport := client.Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != nativeTarget {
			t.Error("caller supplied a native address")
			return nil, net.InvalidAddrError("unexpected native address")
		}
		return new(net.Dialer).DialContext(ctx, network, native.Listener.Addr().String())
	}
	t.Cleanup(client.CloseIdleConnections)
	return ctx, binding, f, source, service, client, state, fact
}

func TestDSGAInitializationActualBindingReadOnlyRecoveryAndMalformedProofs(t *testing.T) {
	ctx, binding, _, _, _, client, state, original := initializationBindingFixture(t)
	before, xmin := sourceFact(t, ctx, binding.server, binding.operationID)
	for range 2 {
		fact, err := binding.observeInitializationWithHTTP(ctx, client)
		if err != nil || fact.Observed.Instance != original.Observed.Instance || fact.Observed.AdmissionOpen {
			t.Fatal("bound native proof unavailable or opened admission")
		}
	}
	after, afterXmin := sourceFact(t, ctx, binding.server, binding.operationID)
	if before != after || xmin != afterXmin {
		t.Fatal("observation rewrote original source facts")
	}
	valid, _ := json.Marshal(original)
	for name, body := range map[string][]byte{
		"duplicate":       []byte(strings.Replace(string(valid), `"protocol_version":1`, `"protocol_version":1,"protocol_version":1`, 1)),
		"missing_proof":   []byte(strings.Replace(string(valid), `"ordinary_roles_verified":true,`, "", 1)),
		"null_proof":      []byte(strings.Replace(string(valid), `"ordinary_roles_verified":true`, `"ordinary_roles_verified":null`, 1)),
		"wrong_vm":        []byte(strings.ReplaceAll(string(valid), original.Startup.VMUID, "recreated-vm")),
		"wrong_operation": []byte(strings.ReplaceAll(string(valid), original.OperationUUID, "other-operation")),
		"number_boot":     []byte(strings.Replace(string(valid), `"pg_boot_id":"17"`, `"pg_boot_id":17`, 1)),
		"oversized":       []byte(strings.Repeat(" ", 65<<10)),
	} {
		t.Run(name, func(t *testing.T) {
			state.mu.Lock()
			state.raw = body
			state.mu.Unlock()
			if _, err := binding.observeInitializationWithHTTP(ctx, client); err == nil {
				t.Fatal("malformed or foreign proof accepted")
			}
		})
	}
	state.mu.Lock()
	state.raw = valid
	state.encoding = "gzip"
	state.mu.Unlock()
	if _, err := binding.observeInitializationWithHTTP(ctx, client); err == nil {
		t.Fatal("unexpected content encoding accepted")
	}
}

func TestDSGAInitializationRejectsDriftBeforeNativeAndExpiredLeaseAfterResponse(t *testing.T) {
	ctx, binding, f, source, service, client, state, _ := initializationBindingFixture(t)
	f.mu.Lock()
	originalVM, originalSource, originalService := f.vm.DeepCopy(), (*source).DeepCopy(), service.DeepCopy()
	f.mu.Unlock()
	for name, mutate := range map[string]func(){
		"vm_uid":           func() { f.vm.UID = "replacement" },
		"config_uid":       func() { f.vm.Spec.DSGABootstrap.ConfigSecretUID = "replacement" },
		"source_seed":      func() { (*source).Data["probeCredentialSeed"][0] ^= 1 },
		"service_owner":    func() { service.OwnerReferences[0].UID = "foreign" },
		"service_uid":      func() { service.UID = "replacement-service" },
		"service_selector": func() { service.Spec.Selector["vm.neon.tech/name"] = "foreign" },
		"service_external": func() { service.Spec.ExternalIPs = []string{"192.0.2.1"} },
		"service_port":     func() { service.Spec.Ports[0].TargetPort = intstr.FromInt(5432) },
	} {
		t.Run(name, func(t *testing.T) {
			f.mu.Lock()
			mutate()
			f.mu.Unlock()
			calls := state.calls.Load()
			if _, err := binding.observeInitializationWithHTTP(ctx, client); err == nil || state.calls.Load() != calls {
				t.Fatal("drift reached native or produced a proof")
			}
			f.mu.Lock()
			f.vm = originalVM.DeepCopy()
			*source = originalSource.DeepCopy()
			*service = *originalService.DeepCopy()
			f.mu.Unlock()
		})
	}
	// 响应生成期间VM完整journal若漂移，不能把检查前的配置和检查后的坏状态
	// 拼成可用回执。恢复同一事实后再验证lease在响应期间失效的拒绝路径。
	state.mu.Lock()
	state.afterRead = func() { f.mu.Lock(); f.vm.Annotations = nil; f.mu.Unlock() }
	state.mu.Unlock()
	if _, err := binding.observeInitializationWithHTTP(ctx, client); err == nil {
		t.Fatal("late journal drift returned usable initialization proof")
	}
	f.mu.Lock()
	f.vm = originalVM.DeepCopy()
	f.mu.Unlock()
	state.mu.Lock()
	state.afterRead = func() {
		if _, err := binding.server.db.Exec(ctx, `UPDATE operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, binding.operationID); err != nil {
			t.Error("test lease expiration failed")
		}
	}
	state.mu.Unlock()
	if _, err := binding.observeInitializationWithHTTP(ctx, client); err == nil {
		t.Fatal("expired worker returned usable initialization proof")
	}
}

// 防止生成的只读创建token被中心通用代理当成Agent执行token使用；Rust门禁另
// 验证它只可进入初始化GET，memory/resize/fence均拒绝。
func TestDSGAInitializationJWTDoesNotGrantGeneralProxyAuthority(t *testing.T) {
	f, _, _, key, _, _ := newBrokerTest(t)
	now := time.Now()
	token := dsgaInitializationJWT(key, brokerTestWorkload, "vm-uid", "compute-1", now)
	var config dsgaCredentialConfig
	if json.Unmarshal(f.config.Data["config.json"], &config) != nil {
		t.Fatal("invalid fixture")
	}
	if _, valid := validDSGAProxyJWT(token, brokerTestWorkload, config, now); valid {
		t.Fatal("read token received general proxy authority")
	}
	// 同一签名/身份改回执行scope后能被通用验证器接受，证明上面的拒绝不是坏key。
	parts := strings.Split(token, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	raw = []byte(strings.Replace(string(raw), "compute_ctl:dsga_initialization", "compute_ctl:dsga", 1))
	if _, valid := validDSGAProxyJWT(proxySigned(key, raw), brokerTestWorkload, config, now); !valid {
		t.Fatal("scope control fixture has a foreign signature or identity")
	}
}
