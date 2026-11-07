package control

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functionedge"
)

func TestFunctionEdgeBrokerRejectsUntrustedAdmissionAndReports(t *testing.T) {
	s := &server{}
	for _, identity := range []string{"", "customer", "neon-function-edge"} {
		r := httptest.NewRequest(http.MethodPost, functionedge.BrokerPath, strings.NewReader("{}"))
		if identity != "" {
			r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Subject: pkix.Name{CommonName: identity}}}}
		}
		w := httptest.NewRecorder()
		s.functionEdgeBrokerHandler().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("unverified caller reached metadata admission")
		}
	}
	t.Setenv("NEON_FUNCTIONS_PUBLIC_ORIGIN", "http://functions.invalid:8792")
	input := functionedge.Admission{Branch: "br_0000000000000004", Slug: "hello", Method: "GET", URI: "/functions/v1/br_0000000000000004/hello/data", Headers: http.Header{}}
	if !validEdgeAdmission(input) {
		t.Fatal("valid request rejected")
	}
	for _, uri := range []string{"https://attacker.invalid/", "/functions/v1/br_0000000000000005/hello/data", "//attacker.invalid/x", "/functions/v1/br_0000000000000004/hello/../secret"} {
		bad := input
		bad.URI = uri
		if validEdgeAdmission(bad) {
			t.Fatal("cross-scope or ambiguous request accepted")
		}
	}
	event := functionEvent{ID: "fre_original", Started: time.Now().Add(-time.Second)}
	complete := functionedge.Completion{EventID: event.ID, Dispatched: true, Status: 200, RequestBytes: 4, ResponseBytes: 8, DurationMS: 500, Outcome: "completed"}
	if !validEdgeCompletion(complete, event, 4) {
		t.Fatal("trusted completion rejected")
	}
	for _, change := range []func(*functionedge.Completion){func(v *functionedge.Completion) { v.EventID = "fre_other" }, func(v *functionedge.Completion) { v.RequestBytes = 5 }, func(v *functionedge.Completion) { v.DurationMS = 900000 }, func(v *functionedge.Completion) { v.ResponseBytes = -1 }, func(v *functionedge.Completion) { v.Dispatched = false }, func(v *functionedge.Completion) { v.Status = 0 }} {
		bad := complete
		change(&bad)
		if validEdgeCompletion(bad, event, 4) {
			t.Fatal("forged completion accepted")
		}
	}
}
