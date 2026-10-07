package control

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// These levels implement the pinned official additive permissions model.
// Account/global roles are deliberately not resource authorization inputs.
func roleLevel(role string) int {
	switch role {
	case "owner", "admin":
		return 3
	case "editor", "developer":
		return 2
	case "viewer":
		return 1
	default:
		return 0
	}
}

func permissionName(level int) string {
	if level >= 3 {
		return "admin"
	}
	if level == 2 {
		return "editor"
	}
	if level == 1 {
		return "viewer"
	}
	return "none"
}

func effectivePermission(organizationRole, projectRole string) int {
	return max(roleLevel(organizationRole), roleLevel(projectRole))
}

type resourceAccess struct {
	OrganizationID, OrganizationRole string
	Level                            int
}
type resourceAccessKey struct{}

func accessFrom(r *http.Request) resourceAccess {
	value, _ := r.Context().Value(resourceAccessKey{}).(resourceAccess)
	return value
}

func (s *server) projectAccess(ctx context.Context, project, actor string) (resourceAccess, error) {
	var a resourceAccess
	var grant string
	err := s.db.QueryRow(ctx, `SELECT p.org_id,m.role,COALESCE(g.role,'')
        FROM projects p JOIN organizations o ON o.id=p.org_id AND o.state='active'
        JOIN organization_members m ON m.org_id=p.org_id AND m.user_id=$2
        LEFT JOIN project_grants g ON g.project_id=p.id AND g.user_id=m.user_id
        WHERE p.id=$1`, project, actor).Scan(&a.OrganizationID, &a.OrganizationRole, &grant)
	a.Level = effectivePermission(a.OrganizationRole, grant)
	return a, err
}

// Authorization is attached to every authenticated route, including reads.
// A missing membership or ungranted collaborator gets 404, never an ID oracle.
func (s *server) authorizeRoute(w http.ResponseWriter, r *http.Request, u user) (*http.Request, bool) {
	project, org := r.PathValue("project"), r.PathValue("org")
	a := resourceAccess{}
	required := 1
	if r.Method != http.MethodGet {
		required = 2
	}
	if project != "" {
		var err error
		a, err = s.projectAccess(r.Context(), project, u.ID)
		if err != nil || a.Level == 0 {
			if err != nil && !isNoRows(err) {
				fail(w, r, 503, "metadata_unavailable", "Could not check project authorization")
			} else {
				fail(w, r, 404, "not_found", "Project not found")
			}
			return r, false
		}
		if strings.Contains(r.URL.Path, "/permissions") || (r.Method == http.MethodDelete && r.PathValue("branch") == "" && r.PathValue("endpoint") == "") {
			required = 3
		}
		if strings.HasSuffix(r.URL.Path, "/protection") || strings.HasSuffix(r.URL.Path, "/recover") {
			required = 3
		}
		var state string
		if err := s.db.QueryRow(r.Context(), "SELECT state FROM projects WHERE id=$1", project).Scan(&state); err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not check lifecycle")
			return r, false
		}
		if state == "deleted" || state == "deleting" || state == "recovering" {
			if !lifecycleReadAllowed(r) || a.Level < 3 {
				fail(w, r, 404, "not_found", "Project not found")
				return r, false
			}
			required = 3
		}
		if strings.HasSuffix(r.URL.Path, "/connection-info") {
			required = 2
		}
		if strings.Contains(r.URL.Path, "/roles") || strings.Contains(r.URL.Path, "/databases") {
			required = 2 // Live catalog reads may cold wake a compute.
		}
	} else if org != "" {
		err := s.db.QueryRow(r.Context(), `SELECT m.role FROM organization_members m
            JOIN organizations o ON o.id=m.org_id AND o.state='active'
            WHERE m.org_id=$1 AND m.user_id=$2`, org, u.ID).Scan(&a.OrganizationRole)
		if err != nil {
			if isNoRows(err) {
				fail(w, r, 404, "not_found", "Organization not found")
			} else {
				fail(w, r, 503, "metadata_unavailable", "Could not check organization authorization")
			}
			return r, false
		}
		a.OrganizationID = org
		a.Level = roleLevel(a.OrganizationRole)
		switch {
		case strings.HasSuffix(r.URL.Path, "/projects") && r.Method == http.MethodGet:
			// Collaborators may list only projects with a positive explicit grant.
			required = 0
		case strings.HasSuffix(r.URL.Path, "/projects") && r.Method == http.MethodPost:
			// Viewers can create their own projects and receive an Admin grant.
			required = 1
		case strings.Contains(r.URL.Path, "/members") || strings.Contains(r.URL.Path, "/api-keys") || strings.Contains(r.URL.Path, "/invitations"):
			required = 3
		case r.Method != http.MethodGet:
			required = 3
		default:
			required = 0
		}
	} else {
		if u.APIKey && (strings.Contains(r.URL.Path, "/api-keys") || r.Method != http.MethodGet) {
			fail(w, r, 403, "forbidden", "API keys cannot manage credentials or accounts")
			return r, false
		}
		return r.WithContext(context.WithValue(r.Context(), resourceAccessKey{}, a)), true
	}
	if u.APIKey {
		if strings.Contains(r.URL.Path, "/invitations") {
			fail(w, r, 403, "forbidden", "Console sessions required for invitations")
			return r, false
		}
		if r.Method != http.MethodGet && roleLevel(u.KeyRole) < 2 {
			fail(w, r, 403, "forbidden", "Read-only API key cannot mutate resources")
			return r, false
		}
		if (u.KeyOrg != "" && u.KeyOrg != a.OrganizationID) ||
			(u.KeyProject != "" && project != "" && project != u.KeyProject) ||
			(u.KeyProject != "" && org != "" && !strings.HasSuffix(r.URL.Path, "/projects")) {
			fail(w, r, 404, "not_found", "Resource not found")
			return r, false
		}
		// Keys are an intersection with the issuer's current permissions.
		// Revoking/demoting membership immediately changes subsequent requests.
		a.Level = min(a.Level, roleLevel(u.KeyRole))
		if strings.Contains(r.URL.Path, "/api-keys") {
			fail(w, r, 403, "forbidden", "API keys cannot manage credentials")
			return r, false
		}
	}
	if a.Level < required {
		fail(w, r, 403, "forbidden", "Insufficient resource permission")
		return r, false
	}
	return r.WithContext(context.WithValue(r.Context(), resourceAccessKey{}, a)), true
}

func (s *server) bearerUser(r *http.Request) (user, error) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == r.Header.Get("Authorization") || !strings.HasPrefix(token, "ncp_") || len(token) > 256 {
		return user{}, errors.New("invalid bearer credential")
	}
	u := user{APIKey: true}
	err := s.db.QueryRow(r.Context(), `SELECT u.id,u.username,u.role,k.id,COALESCE(k.org_id,''),
        COALESCE(k.project_id,''),k.max_role FROM api_keys k JOIN users u ON u.id=k.user_id
        WHERE k.key_hash=$1 AND k.revoked_at IS NULL AND k.expires_at>now() AND NOT u.disabled`,
		digest(token)).Scan(&u.ID, &u.Username, &u.Role, &u.KeyID, &u.KeyOrg, &u.KeyProject, &u.KeyRole)
	return u, err
}
