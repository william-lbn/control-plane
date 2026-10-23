package control

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// Organization management never grants platform authority or access to the
// operator's credential projections. The existing auth layer resolves org ACLs.
func (s *server) requireAIProviderManager(w http.ResponseWriter, r *http.Request) bool {
	if r.PathValue("org") == "" {
		return s.requireAIProviderOperator(w, r)
	}
	if s.aiProviders == nil || !s.aiProviders.policy.AllowOrganizationBYOK {
		fail(w, r, 503, "organization_byok_disabled", "Organization BYOK is not enabled by the operator")
		return false
	}
	if userFrom(r).APIKey || accessFrom(r).Level < 3 {
		fail(w, r, 403, "organization_admin_session_required", "An organization administrator Console session is required")
		return false
	}
	return true
}

func aiProviderOrganizationActor(ctx context.Context, tx pgx.Tx, org, actor string) error {
	var role string
	err := tx.QueryRow(ctx, `SELECT m.role FROM organization_members m JOIN organizations o ON o.id=m.org_id AND o.state='active' JOIN users u ON u.id=m.user_id AND NOT u.disabled WHERE m.org_id=$1 AND m.user_id=$2 FOR SHARE OF m,o,u`, org, actor).Scan(&role)
	if isNoRows(err) || err == nil && roleLevel(role) < 3 {
		return providerProblem(403, "organization_admin_session_required", "Organization administrator authority is no longer active")
	}
	return err
}

func (s *server) aiProviderOrganizationCapabilities(w http.ResponseWriter, r *http.Request) {
	if !s.requireAIProviderManager(w, r) {
		return
	}
	p := s.aiProviders
	jsonResponse(w, 200, record{"enabled": true, "can_manage": true, "credential_sources": []record{}, "raw_secret_input": s.secureCookies && r.TLS != nil && r.TLS.HandshakeComplete, "lab_secret_input": p.policy.AllowLabSecretInput, "protocols": []string{"openai_compatible", "anthropic", "gemini"}, "discovery_performs_inference": false})
}

func (s *server) aiBranchCatalog(ctx context.Context, open bool, branch string) (aiCatalogSnapshot, error) {
	var org string
	err := s.db.QueryRow(ctx, `SELECT p.org_id FROM branches b JOIN projects p ON p.id=b.project_id WHERE b.id=$1`, branch).Scan(&org)
	if err != nil {
		return aiCatalogSnapshot{}, err
	}
	return s.aiCatalogForOrganization(ctx, open, org)
}
