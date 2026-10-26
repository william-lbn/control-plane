package functionedge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

func edgeTLSFixture(t *testing.T, name string) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots.AddCert(parsed)
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{leaf}}, roots
}

func TestIndependentEdgeStreamsDirectGuestAndReportsOnce(t *testing.T) {
	scope := functions.Scope{InstanceID: "fni_0000000000000001", DeploymentID: "fdp_0000000000000002", ProjectID: "prj_0000000000000003", BranchID: "br_0000000000000004", Slug: "hello", Generation: 2}
	key := bytes.Repeat([]byte{7}, 32)
	release := make(chan struct{})
	var calls atomic.Int32
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_neon/status" {
			io.WriteString(w, `{"ready":true}`)
			return
		}
		calls.Add(1)
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-Neon-Spoof") != "" {
			t.Error("public caller injected private headers")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Set-Cookie", "forbidden=1")
		io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "last\n")
	}))
	defer node.Close()
	manager, err := functions.NewManager(scope, key, node.URL, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	managerTLS, roots := edgeTLSFixture(t, scope.InstanceID+".functions.neon.internal")
	guest := httptest.NewUnstartedServer(manager)
	guest.TLS = managerTLS
	guest.StartTLS()
	defer guest.Close()
	statusRequest, _ := http.NewRequest(http.MethodGet, guest.URL+"/internal/v1/status", nil)
	if err = functions.SignManagerRequest(statusRequest, nil, key, time.Now()); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: scope.InstanceID + ".functions.neon.internal"}}}
	statusResponse, err := client.Do(statusRequest)
	if err != nil {
		t.Fatal(err)
	}
	var status functions.InstanceStatus
	if err = json.NewDecoder(statusResponse.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	statusResponse.Body.Close()
	completed := make(chan Completion, 1)
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != BrokerPath {
			t.Error("wrong broker route")
			return
		}
		controller := http.NewResponseController(w)
		if err := controller.EnableFullDuplex(); err != nil {
			t.Error(err)
			return
		}
		decoder := json.NewDecoder(r.Body)
		var input Admission
		if decoder.Decode(&input) != nil {
			t.Error("admission decode")
			return
		}
		v := functions.Invocation{BootID: status.BootID, Generation: scope.Generation, Method: input.Method, URL: "http://functions.invalid" + input.URI, Headers: input.Headers, Body: input.Body}
		body, _ := json.Marshal(v)
		signed, _ := http.NewRequest(http.MethodPost, "https://127.0.0.1:9090/internal/v1/invoke", nil)
		functions.SignManagerRequest(signed, body, key, time.Now())
		grant := Grant{EventID: "fre_direct", RequestID: "fer_direct", DefinitionVersion: 3, Deadline: time.Now().Add(time.Minute),
			Request: functions.DelegatedRequest{Target: "127.0.0.1:9090", Scope: scope, BootID: status.BootID, Path: "/internal/v1/invoke", Body: body, Headers: signed.Header, ExpiresAt: time.Now().Add(30 * time.Second)}}
		json.NewEncoder(w).Encode(grant)
		controller.Flush()
		var complete Completion
		if err := decoder.Decode(&complete); err != nil {
			t.Error(err)
			return
		}
		completed <- complete
		json.NewEncoder(w).Encode(map[string]bool{"completed": true})
		controller.Flush()
	}))
	defer broker.Close()
	origin, _ := url.Parse("http://functions.invalid")
	edge := &edge{origin: origin, broker: broker.URL, brokerClient: broker.Client(), guestRoots: roots,
		dialGuest: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", guest.Listener.Addr().String())
		}}
	mux := http.NewServeMux()
	mux.HandleFunc("/functions/v1/{branch}/{slug}/{rest...}", edge.invoke)
	public := httptest.NewServer(mux)
	defer public.Close()
	public.Client().Timeout = 5 * time.Second
	r, _ := http.NewRequest(http.MethodGet, public.URL+"/functions/v1/"+scope.BranchID+"/hello/data", nil)
	r.Header.Set("Cookie", "console=session")
	r.Header.Set("X-Neon-Spoof", "forged")
	response, err := public.Client().Do(r)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	close(release)
	if err != nil || first != "first\n" {
		t.Fatal("stream was buffered or lost", err)
	}
	rest, err := io.ReadAll(reader)
	response.Body.Close()
	if err != nil || string(rest) != "last\n" || response.Header.Get("Set-Cookie") != "" || response.Header.Get("X-Function-Deployment-ID") != scope.DeploymentID {
		t.Fatal("public response policy changed", err)
	}
	select {
	case v := <-completed:
		if v.EventID != "fre_direct" || !v.Dispatched || v.Status != 200 || v.ResponseBytes != 11 || v.Outcome != "completed" || calls.Load() != 1 {
			t.Fatal("trusted transfer receipt or exact-once dispatch changed", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("edge completion did not reach broker")
	}
}

func TestEdgeHeaderPolicyAndRejectsInvalidGrants(t *testing.T) {
	h := http.Header{"Cookie": {"session"}, "Connection": {"X-Hop"}, "X-Hop": {"private"}, "X-Neon-Manager-Time": {"fake"}, "Authorization": {"customer-bearer"}}
	filtered := IngressHeaders(h)
	if filtered.Get("Cookie") != "" || filtered.Get("X-Hop") != "" || filtered.Get("X-Neon-Manager-Time") != "" || filtered.Get("Authorization") != "customer-bearer" {
		t.Fatal("edge ingress policy changed")
	}
	for _, origin := range []string{"http://functions.invalid", "https://functions.invalid/path", "https://user@functions.invalid", "https://functions.invalid?x=1"} {
		if _, err := NewHandler(Config{PublicOrigin: origin}); err == nil {
			t.Fatal("missing trust accepted")
		}
	}
	input := Admission{Branch: "br_0000000000000004", Slug: "hello", Method: "GET"}
	if validGrant(Grant{}, input, time.Now()) {
		t.Fatal("empty route grant accepted")
	}
	_, _, err := (&edge{}).dispatch(context.Background(), functions.DelegatedRequest{Target: "169.254.169.254:9090"})
	if err == nil || strings.Contains(err.Error(), "169.254") {
		t.Fatal("metadata target accepted or leaked")
	}
}
