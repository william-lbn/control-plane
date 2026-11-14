package control

import (
	"errors"
	"testing"
)

func TestDSGACatalogConnectionRejectsDifferentBranch(t *testing.T) {
	ctx, binding, _, _, _ := dsgaServicesSourceFixture(t, "success")
	s := binding.server
	p := binding.intent.createPayload
	// The production constructor verifies the current runtime scope before
	// touching VM and credential sources. Exercise this through the catalog
	// entry point with the actual frozen Operation from the database fixture.
	runtime, err := s.dsgaEndpointRuntime(ctx, p.ProjectID, p.EndpointID)
	if err != nil || runtime == nil {
		t.Fatalf("expected persisted managed Writer runtime: %v", err)
	}
	wrong := catalogPayload{ProjectID: p.ProjectID, EndpointID: p.EndpointID, BranchID: "br_alternate"}
	conn, err := s.catalogConnection(ctx, wrong)
	if conn != nil || !errors.Is(err, errDSGACreationIdentity) {
		t.Fatal("foreign branch reached the credential or SQL path", err)
	}
}
