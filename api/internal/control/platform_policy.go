package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
)

// Platform operations have an independent explicit approval policy. Being an
// organization administrator, an API-key holder or an AI provider operator is
// not by itself permission to change another organization's service plan.
type platformPolicy struct {
	Version        int      `json:"version"`
	OperatorIDs    []string `json:"operator_ids"`
	Organizations  []string `json:"allowed_organizations,omitempty"`
	PlanManagement bool     `json:"plan_management"`
	operators      map[string]bool
	organizations  map[string]bool
}

func newPlatformPolicy(raw []byte) (*platformPolicy, error) {
	if len(raw) == 0 || len(raw) > 65536 {
		return nil, errors.New("invalid platform approval policy")
	}
	var p platformPolicy
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF || p.Version != 1 || len(p.OperatorIDs) == 0 || len(p.OperatorIDs) > 16 || len(p.Organizations) > 128 {
		return nil, errors.New("invalid platform approval policy")
	}
	p.operators = map[string]bool{}
	p.organizations = map[string]bool{}
	for _, id := range p.OperatorIDs {
		if !aiProviderUserID.MatchString(id) || p.operators[id] {
			return nil, errors.New("invalid platform operator identity")
		}
		p.operators[id] = true
	}
	for _, id := range p.Organizations {
		if !aiProviderUserID.MatchString(id) || p.organizations[id] {
			return nil, errors.New("invalid platform organization scope")
		}
		p.organizations[id] = true
	}
	return &p, nil
}
func loadPlatformPolicy() (*platformPolicy, error) {
	file := os.Getenv("NEON_PLATFORM_POLICY_FILE")
	if file == "" {
		return nil, nil
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, errors.New("approved platform policy unavailable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return nil, errors.New("approved platform policy unavailable")
	}
	return newPlatformPolicy(raw)
}
func (p *platformPolicy) permits(u user, org string) bool {
	return p != nil && p.PlanManagement && !u.APIKey && p.operators[u.ID] && (org == "" || len(p.organizations) == 0 || p.organizations[org])
}
func (s *server) requirePlatformPlans(w http.ResponseWriter, r *http.Request, org string) bool {
	if s.platform == nil || !s.platform.PlanManagement {
		fail(w, r, 503, "platform_plan_management_disabled", "Service plan management requires an approved operator policy")
		return false
	}
	if !s.platform.permits(userFrom(r), org) {
		fail(w, r, 403, "platform_operator_required", "An explicitly approved platform operator session and organization scope are required")
		return false
	}
	return true
}
