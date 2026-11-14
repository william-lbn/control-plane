package control

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
)

func TestDSGAServiceLoginReceiptIsBoundToService(t *testing.T) {
	p := dataAPITestPayload(t)
	p.ProjectID, p.EndpointID, p.Generation = "prj_0123456789abcdef", "ep_0123456789abcdef", 2
	input := dataAPILoginRequest(p, nil)
	input.PGBootID = dsgapi.Uint64(42)
	current := &dsgaEndpointRuntime{Payload: createPayload{ProjectID: p.ProjectID, EndpointID: p.EndpointID}}
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	for _, tc := range []struct {
		name    string
		service string
		ok      bool
	}{{"exact", dsgaDataAPIServiceLogin, true}, {"legacy_auth", "", false}, {"other", "managed_auth", false}} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				var request dsgaAuthLoginRequest
				if err != nil || dsgapi.UnmarshalObject(raw, &request) != nil ||
					request.Service != dsgaDataAPIServiceLogin || request.PGBootID != input.PGBootID || request.Verifier != nil ||
					r.Method != "POST" || r.URL.Path != dsgaAuthLoginPath {
					t.Error("invalid signed service request document")
					w.WriteHeader(400)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				_ = json.NewEncoder(w).Encode(dsgaAuthLoginReceipt{
					ProtocolVersion: 1, Service: tc.service, BranchID: input.BranchID,
					PGBootID: input.PGBootID, Generation: input.Generation,
				})
			}))
			defer endpoint.Close()
			err := requestDSGAAuthLogin(context.Background(), endpoint.Client(), endpoint.URL, key, current, input)
			if (err == nil) != tc.ok {
				t.Fatalf("service %q returned %v", tc.service, err)
			}
		})
	}
}
