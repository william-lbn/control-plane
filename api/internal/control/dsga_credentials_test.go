package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	"github.com/neondatabase/autoscaling/pkg/dsga/owner"
	authentication "k8s.io/api/authentication/v1"
	coordination "k8s.io/api/coordination/v1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const brokerTestWorkload = "cp-0123456789abcdef"

// 本机TLS协议夹具，不是apiserver/RBAC认证。journal的创建/读取/条件更新走
// 自有生产Store及真正生成的typed客户端，不手写压缩日志或放置owner镜像。
type brokerTestAPI struct {
	mu               sync.Mutex
	vm               *vmv1.VirtualMachine
	lease            *coordination.Lease
	config           *core.Secret
	credentials      *core.Secret
	validAPIAuth     bool
	secretWrites     int
	afterKeyRead     func()
	kube             *kubeClient
	listener         dsgaListenerConfig
	proxy            *dsgaProxy
	service          *core.Service
	managementCAFile string
}

func brokerDedicatedCertificate(t *testing.T) ([]byte, tls.Certificate, string, string) {
	t.Helper()
	_, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		DNSNames: []string{"*.neon.svc"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, public, caKey)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	directory := t.TempDir()
	certFile, keyFile := filepath.Join(directory, "issuer.crt"), filepath.Join(directory, "issuer.key")
	if os.WriteFile(certFile, certificate, 0600) != nil || os.WriteFile(keyFile, key, 0600) != nil {
		t.Fatal("cannot create private test identity")
	}
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}), pair, certFile, keyFile
}

func (f *brokerTestAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.validAPIAuth = f.validAPIAuth && r.Header.Get("Authorization") == "Bearer broker-kube-identity"
	w.Header().Set("Content-Type", "application/json")
	if !f.validAPIAuth {
		http.Error(w, "wrong Kubernetes identity", http.StatusForbidden)
		return
	}
	var output any
	switch r.URL.Path {
	case "/apis/vm.neon.tech/v1/namespaces/neon/virtualmachines/" + brokerTestWorkload:
		if r.Method == http.MethodPatch {
			body, err := io.ReadAll(r.Body)
			patch, parseErr := jsonpatch.DecodePatch(body)
			original, marshalErr := json.Marshal(f.vm)
			if err != nil || parseErr != nil || marshalErr != nil {
				http.Error(w, "bad patch", 400)
				return
			}
			updated, err := patch.Apply(original)
			if err != nil || json.Unmarshal(updated, f.vm) != nil {
				http.Error(w, "condition failed", 409)
				return
			}
			version, _ := strconv.Atoi(f.vm.ResourceVersion)
			f.vm.ResourceVersion = strconv.Itoa(version + 1)
		}
		output = f.vm
	case "/apis/coordination.k8s.io/v1/namespaces/neon/leases/" + owner.LeaseName(journal.Binding{Namespace: "neon", Name: brokerTestWorkload, VMUID: "vm-uid", Incarnation: strings.Repeat("a", 32)}):
		if r.Method == http.MethodPut {
			var updated coordination.Lease
			if json.NewDecoder(r.Body).Decode(&updated) != nil || updated.UID != f.lease.UID || updated.ResourceVersion != f.lease.ResourceVersion {
				http.Error(w, "lease condition failed", http.StatusConflict)
				return
			}
			version, _ := strconv.Atoi(updated.ResourceVersion)
			updated.ResourceVersion = strconv.Itoa(version + 1)
			f.lease = updated.DeepCopy()
		}
		output = f.lease
	case "/apis/authentication.k8s.io/v1/tokenreviews":
		var request authentication.TokenReview
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Spec.Token != "projected-agent-token" ||
			len(request.Spec.Audiences) != 1 || request.Spec.Audiences[0] != owner.CredentialAudience {
			http.Error(w, "wrong TokenReview", 403)
			return
		}
		output = &authentication.TokenReview{Status: authentication.TokenReviewStatus{Authenticated: true, Audiences: []string{owner.CredentialAudience},
			User: authentication.UserInfo{Username: "system:serviceaccount:agents:autoscaler", UID: "sa-uid", Extra: map[string]authentication.ExtraValue{
				"authentication.kubernetes.io/pod-name": {"agent-pod"}, "authentication.kubernetes.io/pod-uid": {"pod-uid"},
			}}}}
	case "/api/v1/namespaces/agents/serviceaccounts/autoscaler":
		output = &core.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "autoscaler", Namespace: "agents", UID: "sa-uid", ResourceVersion: "1"}}
	case "/api/v1/namespaces/agents/pods/agent-pod":
		output = &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-pod", Namespace: "agents", UID: "pod-uid", ResourceVersion: "1"},
			Spec: core.PodSpec{NodeName: "node-a", ServiceAccountName: "autoscaler"}, Status: core.PodStatus{Phase: core.PodRunning}}
	case "/api/v1/namespaces/neon/secrets/" + brokerTestWorkload + "-config":
		output = f.config
	case "/api/v1/namespaces/neon/services/" + brokerTestWorkload + "-management":
		output = f.service
	case "/api/v1/namespaces/neon/secrets/" + brokerTestWorkload + "-credentials":
		if r.Method != http.MethodGet {
			f.secretWrites++
		}
		output = f.credentials.DeepCopy()
		if hook := f.afterKeyRead; hook != nil {
			f.afterKeyRead = nil
			// 回调经真实Store向同API条件写journal；不得持本地fixture锁递归网络。
			f.mu.Unlock()
			hook()
			f.mu.Lock()
		}
	default:
		http.Error(w, "unknown resource", 404)
		return
	}
	_ = json.NewEncoder(w).Encode(output)
}

func newBrokerTest(t *testing.T) (*brokerTestAPI, *journal.Store, owner.Ownership, ed25519.PrivateKey, *http.Client, string) {
	t.Helper()
	f := &brokerTestAPI{validAPIAuth: true}
	f.vm = &vmv1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: brokerTestWorkload, Namespace: "neon", UID: "vm-uid", ResourceVersion: "1", Labels: credentialLabels("project-1", "ep_0123456789abcdef")}}
	f.vm.Spec.DSGABootstrap = &vmv1.DSGABootstrap{ProtocolVersion: 1, Incarnation: strings.Repeat("a", 32), ComputeInstanceID: "compute-1", VMUID: "", ConfigSecretUID: "", Ready: false}
	f.config = &core.Secret{ObjectMeta: metav1.ObjectMeta{Name: brokerTestWorkload + "-config", Namespace: "neon", UID: "config-uid", ResourceVersion: "1", Labels: credentialLabels("project-1", "ep_0123456789abcdef")}}
	configOwner := true
	f.config.OwnerReferences = []metav1.OwnerReference{{APIVersion: vmv1.SchemeGroupVersion.String(), Kind: "VirtualMachine", Name: brokerTestWorkload, UID: f.vm.UID, Controller: &configOwner}}
	f.credentials = &core.Secret{ObjectMeta: metav1.ObjectMeta{Name: brokerTestWorkload + "-credentials", Namespace: "neon", UID: "credentials-uid", ResourceVersion: "1", Labels: credentialLabels("project-1", "ep_0123456789abcdef")}}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"spec": map[string]any{"dynamic_shared_buffers": map[string]any{"protocol_version": 2, "vm_uid": "vm-uid", "compute_instance_id": "compute-1",
		"bootstrap_buffer_bytes": 16777216, "maximum_buffer_bytes": 268435456, "minimum_guest_memory_bytes": 2147483648, "non_buffer_reserve_bytes": 1073741824}}}
	installComputeJWK(config, key)
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	f.config.Data = map[string][]byte{"config.json": raw}
	f.credentials.Data = map[string][]byte{"controlSigningSeed": key.Seed()}
	api := httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(api.Close)
	kube := &kubeClient{base: api.URL, namespace: "neon", token: "broker-kube-identity", http: api.Client()}
	f.kube = kube
	broker, err := newDSGACredentialBroker(kube, "agents", "autoscaler")
	if err != nil {
		t.Fatal(err)
	}
	binding := journal.Binding{Namespace: "neon", Name: brokerTestWorkload, VMUID: "vm-uid", Incarnation: strings.Repeat("a", 32)}
	store, err := journal.NewStore(broker.vms.NeonvmV1().VirtualMachines("neon"), binding)
	if err != nil {
		t.Fatal(err)
	}
	holder := "dsga-" + strings.Repeat("b", 32) + ".pod-uid"
	initial, err := store.Initialize(context.Background(), "1", holder)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ReleaseBootstrap(context.Background(), initial.ResourceVersion(), f.config.UID); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.vm.Status.Phase, f.vm.Status.PodName, f.vm.Status.PodIP, f.vm.Status.Node = vmv1.VmRunning, "runner", "10.0.0.1", "node-a"
	controller, duration, transitions, now := true, int32(30), int32(0), metav1.NowMicro()
	f.lease = &coordination.Lease{ObjectMeta: metav1.ObjectMeta{Name: owner.LeaseName(binding), Namespace: "neon", UID: "lease-uid", ResourceVersion: "1",
		Annotations:     map[string]string{"internal.autoscaling.neon.tech/dsga-lease-binding-v1": "1:vm-uid:" + binding.Incarnation},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: vmv1.SchemeGroupVersion.String(), Kind: "VirtualMachine", Name: brokerTestWorkload, UID: binding.VMUID, Controller: &controller}}},
		Spec: coordination.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &duration, LeaseTransitions: &transitions, AcquireTime: &now, RenewTime: &now}}
	f.mu.Unlock()
	claim := owner.Ownership{Namespace: "neon", VMName: brokerTestWorkload, VMUID: "vm-uid", Incarnation: binding.Incarnation, LeaseUID: "lease-uid", HolderID: holder, IssuerEpoch: 1}
	ca, pair, certFile, keyFile := brokerDedicatedCertificate(t)
	f.managementCAFile = filepath.Join(t.TempDir(), "management-ca.crt")
	if os.WriteFile(f.managementCAFile, ca, 0600) != nil {
		t.Fatal("cannot write test management CA")
	}
	gateway := &computeGateway{kube: kube}
	f.listener = dsgaListenerConfig{Bind: "127.0.0.1:0", AgentNamespace: "agents", AgentAccount: "autoscaler", CertFile: certFile, KeyFile: keyFile}
	production, err := gateway.dsgaServer(f.listener)
	if err != nil {
		t.Fatal(err)
	}
	probe, _ := http.NewRequest(http.MethodGet, "/dsg/v1/capabilities", nil)
	handler, _ := production.Handler.(*http.ServeMux).Handler(probe)
	var ok bool
	f.proxy, ok = handler.(*dsgaProxy)
	if !ok {
		t.Fatal("production dedicated listener did not mount DSGA proxy")
	}
	server := httptest.NewUnstartedServer(production.Handler)
	server.TLS = production.TLSConfig.Clone()
	// 命名SNI走真实GetCertificate。显式设同一测试证书防止httptest自动加
	// 另一个localhost证书；IP/no-SNI可验证HTTP第二层拒绝。正式配置没有
	// 默认Certificate，第一层GetCertificate本身就会拒绝未绑定SNI。
	if len(server.TLS.Certificates) != 0 {
		t.Fatal("production listener has a fallback certificate")
	}
	server.TLS.Certificates = []tls.Certificate{pair}
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("invalid test CA")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: brokerTestWorkload + "-management.neon.svc"}}
	t.Cleanup(transport.CloseIdleConnections)
	return f, store, claim, key, &http.Client{Timeout: 5 * time.Second, Transport: transport}, server.URL + "/computes/" + brokerTestWorkload + "/dsga-credentials"
}

func TestDSGAGatewayDefaultClosedAndPartialIdentityFailsStartup(t *testing.T) {
	f, _, _, _, _, _ := newBrokerTest(t)
	gateway := &computeGateway{kube: f.kube}
	mux, err := gateway.routes("", "")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/computes/"+brokerTestWorkload+"/dsga-credentials", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatal("default configuration must not expose the credential route")
	}
	_, pattern := mux.Handler(httptest.NewRequest(http.MethodPost, "/computes/"+brokerTestWorkload+"/configure", nil))
	if pattern == "" {
		t.Fatal("ordinary configure route disappeared")
	}
	for _, subject := range [][2]string{{"agents", ""}, {"", "autoscaler"}} {
		if _, err := gateway.routes(subject[0], subject[1]); err == nil {
			t.Fatal("partial identity must fail startup")
		}
	}
	if server, err := gateway.dsgaServer(dsgaListenerConfig{Bind: ":8444"}); err != nil || server != nil {
		t.Fatal("default must not create an issuer listener")
	}
	for _, alter := range []func(*dsgaListenerConfig){
		func(c *dsgaListenerConfig) { c.AgentNamespace = "" }, func(c *dsgaListenerConfig) { c.AgentAccount = "" },
		func(c *dsgaListenerConfig) { c.CertFile = "" }, func(c *dsgaListenerConfig) { c.KeyFile = "" },
	} {
		config := f.listener
		alter(&config)
		if _, err := gateway.dsgaServer(config); err == nil {
			t.Fatal("partial TLS/subject config must fail startup")
		}
	}
	_, guestCert, guestKey, err := newComputeCertificate("ep_0123456789abcdef", "neon", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	config := f.listener
	config.CertFile = filepath.Join(directory, "guest.crt")
	config.KeyFile = filepath.Join(directory, "guest.key")
	if os.WriteFile(config.CertFile, guestCert, 0600) != nil || os.WriteFile(config.KeyFile, guestKey, 0600) != nil {
		t.Fatal("guest fixture write failed")
	}
	if _, err := gateway.dsgaServer(config); err == nil {
		t.Fatal("Guest-mounted certificate must not authenticate the issuer")
	}
	server, err := gateway.dsgaServer(f.listener)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.TLSConfig.GetCertificate(&tls.ClientHelloInfo{ServerName: ""}); err == nil {
		t.Fatal("production TLS must reject empty SNI before HTTP")
	}
}

func brokerRequest(t *testing.T, client *http.Client, address string, body []byte) (int, []byte) {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer projected-agent-token")
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("credential response is cacheable")
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

func TestDSGABrokerIssuesOnlyBoundShortTokenOverActualTLS(t *testing.T) {
	f, _, claim, key, client, address := newBrokerTest(t)
	body, _ := json.Marshal(claim)
	code, data := brokerRequest(t, client, address, body)
	if code != 200 {
		t.Fatalf("unexpected status %d", code)
	}
	var reply struct {
		AccessToken string `json:"access_token"`
		Epoch       string `json:"issuer_epoch"`
		Instance    string `json:"compute_instance_id"`
	}
	if json.Unmarshal(data, &reply) != nil || reply.Epoch != "1" || reply.Instance != "compute-1" {
		t.Fatal("wrong binding response")
	}
	parts := strings.Split(reply.AccessToken, ".")
	if len(parts) != 3 {
		t.Fatal("not a JWT")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(key.Public().(ed25519.PublicKey), []byte(parts[0]+"."+parts[1]), sig) {
		t.Fatal("bad signature")
	}
	claimsRaw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if json.Unmarshal(claimsRaw, &claims) != nil || claims["aud"] != "neon-dsga" || claims["scope"] != "compute_ctl:dsga" ||
		claims["compute_id"] != brokerTestWorkload || claims["vm_uid"] != "vm-uid" || claims["compute_instance_id"] != "compute-1" || claims["issuer_epoch"] != "1" {
		t.Fatal("wrong dedicated claims")
	}
	lifetime := claims["exp"].(float64) - claims["nbf"].(float64)
	if lifetime <= 0 || lifetime > 300 {
		t.Fatal("unbounded token")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.validAPIAuth || f.secretWrites != 0 {
		t.Fatal("confused Kubernetes identity or unexpected key write")
	}
}

func TestDSGABrokerRequiresReadyOriginalBootstrapAndConfigUID(t *testing.T) {
	for _, scenario := range []string{"config_recreated", "config_owner", "bootstrap_compute", "bootstrap_config", "bootstrap_missing", "bootstrap_held", "changed_after_key"} {
		t.Run(scenario, func(t *testing.T) {
			f, _, claim, _, client, address := newBrokerTest(t)
			f.mu.Lock()
			switch scenario {
			case "config_recreated":
				f.config.UID = "recreated-config"
			case "config_owner":
				f.config.OwnerReferences[0].UID = "foreign-vm"
			case "bootstrap_compute":
				f.vm.Spec.DSGABootstrap.ComputeInstanceID = "different-compute"
			case "bootstrap_config":
				f.vm.Spec.DSGABootstrap.ConfigSecretUID = "different-config"
			case "bootstrap_missing":
				f.vm.Spec.DSGABootstrap = nil
			case "bootstrap_held":
				f.vm.Spec.DSGABootstrap.Ready = false
			case "changed_after_key":
				f.afterKeyRead = func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					f.vm.Spec.DSGABootstrap.ConfigSecretUID = "changed-after-key"
					f.vm.ResourceVersion += "x"
				}
			}
			f.mu.Unlock()
			body, _ := json.Marshal(claim)
			code, data := brokerRequest(t, client, address, body)
			if code != http.StatusForbidden || bytes.Contains(data, []byte("access_token")) {
				t.Fatalf("bootstrap %s unexpectedly minted (%d)", scenario, code)
			}
		})
	}
}

func TestDSGABrokerRejectsForeignConfigKeysAndChangedOwner(t *testing.T) {
	f, store, claim, _, client, address := newBrokerTest(t)
	body, _ := json.Marshal(claim)
	for _, test := range []struct {
		name   string
		change func()
	}{
		{"foreign-config", func() { f.config.Labels["neon-control/project-id"] = "other" }},
		{"missing-seed", func() { delete(f.credentials.Data, "controlSigningSeed") }},
		{"wrong-seed", func() { f.credentials.Data["controlSigningSeed"] = bytes.Repeat([]byte{1}, 32) }},
		{"wrong-jwk-purpose", func() {
			f.config.Data["config.json"] = bytes.Replace(f.config.Data["config.json"], []byte(`"use":"sig"`), []byte(`"use":"enc"`), 1)
		}},
		{"wrong-jwk-operation", func() {
			f.config.Data["config.json"] = bytes.Replace(f.config.Data["config.json"], []byte(`"key_ops":["verify"]`), []byte(`"key_ops":["sign"]`), 1)
		}},
		{"wrong-jwk-public-key", func() {
			key := ed25519.NewKeyFromSeed(f.credentials.Data["controlSigningSeed"])
			correct := base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
			foreign := base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
			f.config.Data["config.json"] = bytes.Replace(f.config.Data["config.json"], []byte(`"x":"`+correct+`"`), []byte(`"x":"`+foreign+`"`), 1)
		}},
		{"foreign-vm", func() {
			f.config.Data["config.json"] = bytes.Replace(f.config.Data["config.json"], []byte(`"vm_uid":"vm-uid"`), []byte(`"vm_uid":"other-vm"`), 1)
		}},
		{"old-protocol", func() {
			f.config.Data["config.json"] = bytes.Replace(f.config.Data["config.json"], []byte(`"protocol_version":2`), []byte(`"protocol_version":1`), 1)
		}},
		{"missing-instance", func() {
			f.config.Data["config.json"] = bytes.Replace(f.config.Data["config.json"], []byte(`"compute_instance_id":"compute-1"`), []byte(`"compute_instance_id":""`), 1)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.mu.Lock()
			cfg, credential := f.config.DeepCopy(), f.credentials.DeepCopy()
			test.change()
			f.mu.Unlock()
			code, data := brokerRequest(t, client, address, body)
			if code != 403 || bytes.Contains(data, []byte("access_token")) {
				t.Fatal("invalid identity issued credentials")
			}
			f.mu.Lock()
			f.config, f.credentials = cfg, credential
			f.mu.Unlock()
			code, _ = brokerRequest(t, client, address, body)
			if code != 200 {
				t.Fatal("restored original identity must be independently authorized")
			}
		})
	}
	f.mu.Lock()
	f.afterKeyRead = func() {
		snapshot, err := store.Load(context.Background())
		if err != nil {
			t.Error(err)
			return
		}
		_, err = store.Takeover(context.Background(), snapshot, snapshot.State().Owner, "other-owner")
		if err != nil {
			t.Error(err)
		}
	}
	f.mu.Unlock()
	code, data := brokerRequest(t, client, address, body)
	if code != 403 || bytes.Contains(data, []byte("access_token")) {
		t.Fatal("owner changed during private key read must prevent issuing")
	}
}

func TestDSGABrokerRejectsMalformedRequestsAndVerifiedTLSWithoutBoundSNI(t *testing.T) {
	_, _, claim, _, client, address := newBrokerTest(t)
	body, _ := json.Marshal(claim)
	for _, test := range []struct {
		name string
		body []byte
		code int
	}{
		{"bad-json", []byte(`{`), 400},
		{"unknown-field", []byte(strings.TrimSuffix(string(body), "}") + `,"admin":true}`), 400},
		{"two-documents", append(append([]byte{}, body...), []byte(` {}`)...), 400},
		{"oversized-whitespace", append(append([]byte{}, body...), bytes.Repeat([]byte{' '}, 8193)...), 400},
		{"numeric-epoch", bytes.Replace(body, []byte(`"issuer_epoch":"1"`), []byte(`"issuer_epoch":1`), 1), 400},
		{"self-reported-epoch", bytes.Replace(body, []byte(`"issuer_epoch":"1"`), []byte(`"issuer_epoch":"2"`), 1), 403},
		{"foreign-namespace", bytes.Replace(body, []byte(`"namespace":"neon"`), []byte(`"namespace":"other"`), 1), 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, reply := brokerRequest(t, client, address, test.body)
			if code != test.code || bytes.Contains(reply, []byte("access_token")) {
				t.Fatalf("unexpected status %d", code)
			}
		})
	}
	r, err := http.NewRequest(http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Add("Authorization", "Bearer projected-agent-token")
	r.Header.Add("Authorization", "Bearer projected-agent-token")
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("duplicate Authorization must be rejected")
	}
	// 证书也包含127.0.0.1 SAN：依然验证同一CA且握手成功，但IP方式不发送
	// Compute SNI。入口须拒绝，不能用InsecureSkipVerify掩盖测试中的TLS失败。
	transport := client.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	defer transport.CloseIdleConnections()
	noSNI := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	code, reply := brokerRequest(t, noSNI, address, body)
	if code != 403 || bytes.Contains(reply, []byte("access_token")) {
		t.Fatal("verified TLS without exact Compute SNI must not issue")
	}
}
