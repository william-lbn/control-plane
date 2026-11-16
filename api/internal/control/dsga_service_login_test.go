package control

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
			_, err := requestDSGAAuthLogin(context.Background(), endpoint.Client(), endpoint.URL, key, current, input)
			if (err == nil) != tc.ok {
				t.Fatalf("service %q returned %v", tc.service, err)
			}
		})
	}
}

func TestDSGARealtimeLoginReceiptBindsOriginalIdentity(t *testing.T) {
	relations := []dsgaRealtimeRelation{}
	input := dsgaAuthLoginRequest{
		ProtocolVersion: 1, Service: dsgaRealtimeServiceLogin, ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef",
		EndpointID: "ep_0123456789abcdef", PGBootID: 42, Database: "neondb", Generation: 3,
		RealtimeID: "rts_1234567890abcdef12345678", Relations: &relations,
	}
	current := &dsgaEndpointRuntime{Payload: createPayload{ProjectID: input.ProjectID, EndpointID: input.EndpointID}}
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	for _, returned := range []string{input.RealtimeID, "rts_1234567890abcdef12345679"} {
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var document dsgaAuthLoginRequest
			raw, err := io.ReadAll(r.Body)
			if err != nil || dsgapi.UnmarshalObject(raw, &document) != nil || document.RealtimeID != input.RealtimeID ||
				document.Relations == nil || len(*document.Relations) != 0 || document.Verifier != nil {
				t.Error("Replication identity or empty relation set was lost")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(dsgaAuthLoginReceipt{ProtocolVersion: 1, Service: input.Service,
				BranchID: input.BranchID, PGBootID: input.PGBootID, RealtimeID: returned, Generation: input.Generation})
		}))
		_, err := requestDSGAAuthLogin(context.Background(), peer.Client(), peer.URL, key, current, input)
		peer.Close()
		if (err == nil) != (returned == input.RealtimeID) {
			t.Fatalf("native login accepted identity %q with result %v", returned, err)
		}
	}
}

func TestDSGACatalogRoleReceiptBindsOriginalOperationAndOID(t *testing.T) {
	intent := dsgaCatalogRoleIntent{OperationID: "op_000000000000000000000004", Action: "delete",
		Name: "customer test", ExpectedOID: 16395}
	input := dsgaAuthLoginRequest{ProtocolVersion: 1, Service: dsgaCatalogRoleServiceLogin,
		ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", EndpointID: "ep_0123456789abcdef",
		PGBootID: 42, Database: "postgres", Generation: 3, CatalogRole: &intent}
	current := &dsgaEndpointRuntime{Payload: createPayload{ProjectID: input.ProjectID, EndpointID: input.EndpointID}}
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	good := dsgaCatalogRoleReceipt{OperationID: intent.OperationID, Action: intent.Action, OID: intent.ExpectedOID}
	for _, tc := range []struct {
		name    string
		role    *dsgaCatalogRoleReceipt
		boot    dsgapi.Uint64
		allowed bool
	}{
		{"exact", &good, input.PGBootID, true},
		{"missing", nil, input.PGBootID, false},
		{"wrong_boot", &good, input.PGBootID + 1, false},
		{"wrong_operation", &dsgaCatalogRoleReceipt{"op_000000000000000000000005", good.Action, good.OID}, input.PGBootID, false},
		{"wrong_oid", &dsgaCatalogRoleReceipt{good.OperationID, good.Action, good.OID + 1}, input.PGBootID, false},
		{"wrong_action", &dsgaCatalogRoleReceipt{good.OperationID, "rotate", good.OID}, input.PGBootID, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var sent dsgaAuthLoginRequest
				body, err := io.ReadAll(r.Body)
				if err != nil || dsgapi.UnmarshalObject(body, &sent) != nil ||
					sent.CatalogRole == nil || *sent.CatalogRole != intent || sent.Verifier != nil {
					t.Error("request differs from role intent")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				_ = json.NewEncoder(w).Encode(dsgaAuthLoginReceipt{ProtocolVersion: 1, Service: input.Service,
					BranchID: input.BranchID, PGBootID: tc.boot, Generation: input.Generation, CatalogRole: tc.role})
			}))
			defer peer.Close()
			receipt, err := requestDSGAAuthLogin(context.Background(), peer.Client(), peer.URL, key, current, input)
			if tc.allowed {
				if err != nil || receipt.CatalogRole == nil || *receipt.CatalogRole != good {
					t.Fatal("exact receipt not returned", err)
				}
			} else if err == nil || receipt.CatalogRole != nil {
				t.Fatal("mismatched original receipt was accepted", receipt)
			}
		})
	}
	for _, name := range []string{"app_owner", "PG_signal_backend", "rtP_example", "dsga_catalog_role_m", "/.*"} {
		reserved := intent
		reserved.Name = name
		if reserved.valid(nil) {
			t.Errorf("reserved role %q was accepted", name)
		}
	}
}

func TestDSGAReaderCatalogRefreshBindsWriterHistory(t *testing.T) {
	verifier := dsgaRenderFixture().ApplicationVerifier
	intent := dsgaCatalogRoleIntent{OperationID: "op_000000000000000000000004", Action: "create",
		Name: "customer test", ExpectedOID: 16395}
	input := dsgaAuthLoginRequest{ProtocolVersion: 1, Service: dsgaCatalogRoleRefreshLogin,
		ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", EndpointID: "ep_0123456789abcdef",
		TenantID: strings.Repeat("a", 32), TimelineID: strings.Repeat("b", 32),
		PGBootID: 42, Database: "postgres", Generation: 3, CatalogRole: &intent, Verifier: &verifier}
	role := dsgaCatalogRoleReceipt{OperationID: intent.OperationID, Action: intent.Action, OID: intent.ExpectedOID}
	good := dsgaAuthLoginReceipt{ProtocolVersion: 1, Service: input.Service, BranchID: input.BranchID,
		TenantID: input.TenantID, TimelineID: input.TimelineID, PGBootID: input.PGBootID,
		Generation: input.Generation, CatalogRole: &role, Enabled: true}
	if !validDSGAReaderCatalogRefresh(input) || intent.valid(input.Verifier) {
		t.Fatal("Reader CREATE must require the original observed OID and Writer CREATE must reject it")
	}
	for _, tc := range []struct {
		name   string
		change func(*dsgaAuthLoginReceipt)
	}{
		{"exact", nil},
		{"missing timeline", func(out *dsgaAuthLoginReceipt) { out.TimelineID = "" }},
		{"other timeline", func(out *dsgaAuthLoginReceipt) { out.TimelineID = out.TenantID }},
		{"other tenant", func(out *dsgaAuthLoginReceipt) { out.TenantID = out.TimelineID }},
		{"Writer service", func(out *dsgaAuthLoginReceipt) { out.Service = dsgaCatalogRoleServiceLogin }},
		{"other generation", func(out *dsgaAuthLoginReceipt) { out.Generation-- }},
		{"other boot", func(out *dsgaAuthLoginReceipt) { out.PGBootID++ }},
		{"different OID", func(out *dsgaAuthLoginReceipt) { out.CatalogRole.OID++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := good
			copiedRole := role
			out.CatalogRole = &copiedRole
			if tc.change != nil {
				tc.change(&out)
			}
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var sent dsgaAuthLoginRequest
				body, err := io.ReadAll(r.Body)
				if err != nil || dsgapi.UnmarshalObject(body, &sent) != nil || !validDSGAReaderCatalogRefresh(sent) ||
					sent.TenantID != input.TenantID || sent.TimelineID != input.TimelineID || sent.EndpointID != input.EndpointID ||
					sent.Service != input.Service || *sent.CatalogRole != intent {
					t.Error("invalid signed Reader catalog scope")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				_ = json.NewEncoder(w).Encode(out)
			}))
			defer peer.Close()
			key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
			_, err := requestDSGAAuthLogin(context.Background(), peer.Client(), peer.URL, key, &dsgaEndpointRuntime{}, input)
			if (err == nil) != (tc.change == nil) {
				t.Fatal("receipt outcome differs from the signed Reader scope", err)
			}
		})
	}
	intent.ExpectedOID = 0
	if validDSGAReaderCatalogRefresh(input) {
		t.Fatal("Reader accepted an unobserved Writer OID")
	}
	intent.ExpectedOID, intent.Action = 16395, "delete"
	if validDSGAReaderCatalogRefresh(input) {
		t.Fatal("Reader delete accepted a verifier")
	}
	input.Verifier = nil
	if !validDSGAReaderCatalogRefresh(input) {
		t.Fatal("Reader rejected the exact tombstone")
	}
}
