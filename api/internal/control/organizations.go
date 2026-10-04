package control

import (
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var accountName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@+-]{2,127}$`)

func displayName(value string) bool {
	if strings.TrimSpace(value) != value || utf8.RuneCountInString(value) < 1 || utf8.RuneCountInString(value) > 80 {
		return false
	}
	for _, ch := range value {
		if unicode.IsControl(ch) {
			return false
		}
	}
	return true
}

func organizationRole(value string) bool {
	return value == "admin" || value == "editor" || value == "viewer" || value == "collaborator"
}

func (s *server) organizations(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if r.Method == http.MethodGet {
		rows, err := s.many(r.Context(), `SELECT o.id,o.slug,o.name,o.state,o.created_at,m.role
            FROM organizations o JOIN organization_members m ON m.org_id=o.id
            WHERE m.user_id=$1 AND o.state='active' AND ($2='' OR o.id=$2) ORDER BY o.created_at,o.id`, u.ID, u.KeyOrg)
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not list organizations")
			return
		}
		jsonResponse(w, 200, page(rows))
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if readJSON(r, &body) != nil || !displayName(body.Name) {
		fail(w, r, 422, "invalid_organization", "Organization name required (1-80 characters)")
		return
	}
	id := newID("org_")
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Transaction unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	_, err = tx.Exec(r.Context(), `INSERT INTO organizations(id,slug,name) VALUES($1,$1,$2)`, id, body.Name)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO organization_members(org_id,user_id,role) VALUES($1,$2,'admin')`, id, u.ID)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO organization_quotas(org_id) VALUES($1)`, id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not create organization")
		return
	}
	item, _ := s.one(r.Context(), `SELECT id,slug,name,state,created_at FROM organizations WHERE id=$1`, id)
	item["role"] = "admin"
	s.audit(r.Context(), u.ID, "create_organization", "organization", id, "succeeded", requestID(r))
	jsonResponse(w, 201, item)
}

func (s *server) organizationDetail(w http.ResponseWriter, r *http.Request) {
	row, err := s.one(r.Context(), `SELECT o.id,o.slug,o.name,o.state,o.created_at,q.max_projects,q.max_endpoints
        FROM organizations o JOIN organization_quotas q ON q.org_id=o.id WHERE o.id=$1`, r.PathValue("org"))
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read organization")
		return
	}
	row["role"] = accessFrom(r).OrganizationRole
	jsonResponse(w, 200, row)
}

func (s *server) organizationMembers(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	if r.Method == http.MethodGet {
		rows, err := s.many(r.Context(), `SELECT m.user_id,u.username,m.role,m.created_at FROM organization_members m
            JOIN users u ON u.id=m.user_id WHERE m.org_id=$1 ORDER BY m.created_at,m.user_id`, org)
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not list members")
			return
		}
		jsonResponse(w, 200, page(rows))
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if readJSON(r, &body) != nil || !accountName.MatchString(body.Username) || !organizationRole(body.Role) {
		fail(w, r, 422, "invalid_member", "Valid username and organization role required")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Transaction unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	// Serialize membership mutations and recheck the actor inside the transaction.
	var locked, actorRole string
	err = tx.QueryRow(r.Context(), `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, org).Scan(&locked)
	if err == nil {
		err = tx.QueryRow(r.Context(), `SELECT role FROM organization_members WHERE org_id=$1 AND user_id=$2`, org, userFrom(r).ID).Scan(&actorRole)
	}
	if err != nil || roleLevel(actorRole) < 3 {
		fail(w, r, 403, "forbidden", "Organization Admin permission required")
		return
	}
	var uid string
	err = tx.QueryRow(r.Context(), `SELECT id FROM users WHERE username=$1 AND NOT disabled`, body.Username).Scan(&uid)
	if isNoRows(err) {
		if len(body.Password) < 12 || len(body.Password) > 256 {
			fail(w, r, 422, "initial_password_required", "New local accounts require a 12-256 character password")
			return
		}
		uid = newID("usr_")
		_, err = tx.Exec(r.Context(), `INSERT INTO users(id,username,password_hash,role) VALUES($1,$2,$3,'viewer')`, uid, body.Username, hashPassword(body.Password))
	} else if err == nil && body.Password != "" {
		// An organization Admin cannot reset a shared account's global password.
		fail(w, r, 422, "existing_account_password_forbidden", "Omit password when adding an existing account")
		return
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO organization_members(org_id,user_id,role) VALUES($1,$2,$3)`, org, uid, body.Role)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		status, code, msg := conflictCode(err)
		fail(w, r, status, code, msg)
		return
	}
	s.audit(r.Context(), userFrom(r).ID, "add_organization_member", "organization", org, "succeeded", requestID(r))
	jsonResponse(w, 201, record{"user_id": uid, "username": body.Username, "role": body.Role})
}

func (s *server) changeMember(w http.ResponseWriter, r *http.Request) {
	org, uid := r.PathValue("org"), r.PathValue("member")
	var body struct {
		Role string `json:"role"`
	}
	if r.Method != http.MethodDelete && (readJSON(r, &body) != nil || !organizationRole(body.Role)) {
		fail(w, r, 422, "invalid_member_role", "Admin, Editor, Viewer or Collaborator role required")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Transaction unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var locked, actorRole, previous string
	err = tx.QueryRow(r.Context(), `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, org).Scan(&locked)
	if err == nil {
		err = tx.QueryRow(r.Context(), `SELECT role FROM organization_members WHERE org_id=$1 AND user_id=$2`, org, userFrom(r).ID).Scan(&actorRole)
	}
	if err != nil || roleLevel(actorRole) < 3 {
		fail(w, r, 403, "forbidden", "Organization Admin permission required")
		return
	}
	err = tx.QueryRow(r.Context(), `SELECT role FROM organization_members WHERE org_id=$1 AND user_id=$2`, org, uid).Scan(&previous)
	if isNoRows(err) {
		fail(w, r, 404, "not_found", "Member not found")
		return
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read member")
		return
	}
	if roleLevel(previous) == 3 && roleLevel(body.Role) < 3 {
		var admins int
		err = tx.QueryRow(r.Context(), `SELECT count(*) FROM organization_members WHERE org_id=$1 AND role IN ('owner','admin')`, org).Scan(&admins)
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not check last Admin")
			return
		}
		if admins <= 1 {
			fail(w, r, 409, "last_admin", "At least one organization Admin must remain")
			return
		}
	}
	action := "update_organization_member"
	if r.Method == http.MethodDelete {
		action = "remove_organization_member"
		_, err = tx.Exec(r.Context(), `DELETE FROM project_grants g USING projects p WHERE g.project_id=p.id AND p.org_id=$1 AND g.user_id=$2`, org, uid)
		if err == nil {
			_, err = tx.Exec(r.Context(), `DELETE FROM organization_members WHERE org_id=$1 AND user_id=$2`, org, uid)
		}
		if err == nil {
			_, err = tx.Exec(r.Context(), `UPDATE api_keys SET revoked_at=now() WHERE org_id=$1 AND user_id=$2 AND revoked_at IS NULL`, org, uid)
		}
	} else {
		_, err = tx.Exec(r.Context(), `UPDATE organization_members SET role=$3 WHERE org_id=$1 AND user_id=$2`, org, uid, body.Role)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not change member")
		return
	}
	s.audit(r.Context(), userFrom(r).ID, action, "organization", org, "succeeded", requestID(r))
	jsonResponse(w, 200, record{"user_id": uid, "role": body.Role, "removed": r.Method == http.MethodDelete})
}

func (s *server) projectPermissions(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if r.Method == http.MethodGet {
		rows, err := s.many(r.Context(), `SELECT m.user_id,u.username,m.role AS organization_role,COALESCE(g.role,'') AS project_role
            FROM projects p JOIN organization_members m ON m.org_id=p.org_id JOIN users u ON u.id=m.user_id
            LEFT JOIN project_grants g ON g.project_id=p.id AND g.user_id=m.user_id WHERE p.id=$1 ORDER BY u.username`, project)
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not read project permissions")
			return
		}
		for _, row := range rows {
			row["effective_permission"] = permissionName(effectivePermission(stringVal(row["organization_role"]), stringVal(row["project_role"])))
		}
		jsonResponse(w, 200, page(rows))
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if r.Method != http.MethodDelete && (readJSON(r, &body) != nil || roleLevel(body.Role) < 1 || body.Role == "owner" || body.Role == "developer") {
		fail(w, r, 422, "invalid_permission", "Viewer, Editor or Admin grant required")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Transaction unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var org string
	err = tx.QueryRow(r.Context(), `SELECT org_id FROM projects WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, project).Scan(&org)
	var member bool
	if err == nil {
		err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM organization_members WHERE org_id=$1 AND user_id=$2)`, org, r.PathValue("member")).Scan(&member)
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not check project member")
		return
	}
	if !member {
		fail(w, r, 404, "not_found", "Organization member not found")
		return
	}
	if r.Method == http.MethodDelete {
		_, err = tx.Exec(r.Context(), `DELETE FROM project_grants WHERE project_id=$1 AND user_id=$2`, project, r.PathValue("member"))
	} else {
		_, err = tx.Exec(r.Context(), `INSERT INTO project_grants(project_id,user_id,role) VALUES($1,$2,$3)
        ON CONFLICT(project_id,user_id) DO UPDATE SET role=EXCLUDED.role`, project, r.PathValue("member"), body.Role)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not change permission")
		return
	}
	s.audit(r.Context(), userFrom(r).ID, "change_project_permission", "project", project, "succeeded", requestID(r))
	jsonResponse(w, 200, record{"user_id": r.PathValue("member"), "project_id": project, "role": body.Role, "revoked": r.Method == http.MethodDelete})
}

func (s *server) apiKeys(w http.ResponseWriter, r *http.Request) {
	org, u := r.PathValue("org"), userFrom(r)
	if r.Method == http.MethodGet {
		rows, err := s.many(r.Context(), `SELECT id,name,user_id,org_id,project_id,max_role,created_at,expires_at,revoked_at
			FROM api_keys WHERE ($1<>'' AND org_id=$1) OR ($1='' AND user_id=$2 AND org_id IS NULL) ORDER BY created_at DESC LIMIT 200`, org, u.ID)
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not list API keys")
			return
		}
		jsonResponse(w, 200, page(rows))
		return
	}
	if r.Method == http.MethodDelete {
		tag, err := s.db.Exec(r.Context(), `UPDATE api_keys SET revoked_at=COALESCE(revoked_at,now()) WHERE id=$1
			AND (($2<>'' AND org_id=$2) OR ($2='' AND user_id=$3 AND org_id IS NULL))`, r.PathValue("key"), org, u.ID)
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not revoke API key")
			return
		}
		if tag.RowsAffected() != 1 {
			fail(w, r, 404, "not_found", "API key not found")
			return
		}
		s.audit(r.Context(), u.ID, "revoke_api_key", "api_key", r.PathValue("key"), "succeeded", requestID(r))
		jsonResponse(w, 200, record{"revoked": true})
		return
	}
	var body struct {
		Name        string `json:"name"`
		ProjectID   string `json:"project_id"`
		MaxRole     string `json:"max_role"`
		ExpiresDays int    `json:"expires_days"`
	}
	if readJSON(r, &body) != nil || !displayName(body.Name) || body.ExpiresDays < 1 || body.ExpiresDays > 365 ||
		(body.MaxRole != "viewer" && body.MaxRole != "editor" && body.MaxRole != "admin") || (org == "" && body.ProjectID != "") {
		fail(w, r, 422, "invalid_api_key", "Name, Viewer/Editor/Admin ceiling and expiry 1-365 days required; scoped keys require an organization")
		return
	}
	if body.ProjectID != "" {
		a, err := s.projectAccess(r.Context(), body.ProjectID, u.ID)
		if err != nil || a.OrganizationID != org {
			fail(w, r, 404, "not_found", "Project not found in organization")
			return
		}
	}
	id, token := newID("key_"), "ncp_"+randomToken(48)
	expiry := time.Now().UTC().Add(time.Duration(body.ExpiresDays) * 24 * time.Hour)
	row, err := s.one(r.Context(), `INSERT INTO api_keys(id,user_id,org_id,project_id,name,key_hash,max_role,expires_at)
        VALUES($1,$2,NULLIF($3,''),NULLIF($4,''),$5,$6,$7,$8)
        RETURNING id,name,org_id,project_id,max_role,created_at,expires_at`, id, u.ID, org, body.ProjectID, body.Name, digest(token), body.MaxRole, expiry)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not create API key")
		return
	}
	row["token"] = token // Returned exactly once. Lists contain metadata only.
	s.audit(r.Context(), u.ID, "create_api_key", "api_key", id, "succeeded", requestID(r))
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, 201, row)
}
