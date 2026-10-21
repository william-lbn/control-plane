package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/functionsql"
)

type defaultOwnerContext struct {
	ProjectID, BranchID, EndpointID, PlatformRole, Database string
	Scope                                                   catalogNativeScope
}
type defaultOwnerBinding struct {
	Database, PlatformRole, InitialOwner, CommentSHA256, Mode, Owner, State, OperationID, LastOperationID, Basis string
	DatabaseOID, PlatformRoleOID, InitialOwnerOID, OwnerOID                                                      uint32
	Scope                                                                                                        catalogNativeScope
	Flags                                                                                                        catalogDatabaseFlags
	RoleVersion, Version                                                                                         int64
	CredentialRef, CredentialUID                                                                                 string
}
type defaultOwnerRoleProof struct {
	Name, CredentialRef, CredentialUID string
	OID                                uint32
	Version                            int64
	Flags                              catalogRoleFlags
	MarkerSHA256, VerifierSHA256       string
}
type defaultOwnerInput struct {
	Mode             string `json:"mode"`
	Owner            string `json:"owner,omitempty"`
	OwnerOID         uint32 `json:"owner_oid,omitempty"`
	DatabaseOID      uint32 `json:"expected_database_oid"`
	PreviousOwnerOID uint32 `json:"expected_previous_owner_oid"`
	Acknowledge      bool   `json:"acknowledge_owner_change"`
	Initial          bool   `json:"acknowledge_initial_observation"`
	Password         string `json:"password"`
}

func (v defaultOwnerInput) valid() bool {
	return v.Acknowledge && v.DatabaseOID > 0 && v.PreviousOwnerOID > 0 && v.Password != "" && len(v.Password) <= 256 && (v.Mode == "platform" && v.Owner == "" && v.OwnerOID == 0 || v.Mode == "managed" && validPGIdentifier(v.Owner) && !protectedPGRole(v.Owner) && v.OwnerOID > 0)
}

type defaultOwnerPayload struct {
	catalogPayload
	Native                  defaultOwnerSQLIntent `json:"native"`
	Mode                    string                `json:"mode"`
	Initial                 bool                  `json:"initial_observation"`
	TargetRoleVersion       int64                 `json:"target_role_version"`
	TargetCredentialRef     string                `json:"target_credential_ref,omitempty"`
	TargetCredentialUID     string                `json:"target_credential_uid,omitempty"`
	BootstrapCredentialUID  string                `json:"bootstrap_credential_uid"`
	BootstrapVerifierSHA256 string                `json:"bootstrap_verifier_sha256"`
	ProbeVerifierSHA256     string                `json:"probe_verifier_sha256"`
	TargetVerifierSHA256    string                `json:"target_verifier_sha256,omitempty"`
	ConfigUIDs              map[string]string     `json:"config_uids"`
	RoleHashes              map[string]string     `json:"role_hashes"`
	ColdOwners              map[string]string     `json:"cold_owners"`
	RoutesUID               string                `json:"routes_uid"`
	LegacyDatabaseVersion   int64                 `json:"legacy_database_version,omitempty"`
}

func (p defaultOwnerPayload) valid() bool {
	if !p.Native.valid() || !defaultOwnerProjectID.MatchString(p.ProjectID) || !defaultOwnerBranchID.MatchString(p.BranchID) || !functionWriterEndpointID.MatchString(p.EndpointID) || p.ExpectedVersion < 1 || p.ExpectedVersion >= 9223372036854775806 || p.LegacyDatabaseVersion < 0 || p.Name != p.Native.Database || p.DatabaseOID != p.Native.DatabaseOID || p.PreviousOwner != p.Native.PreviousOwner || p.Owner != p.Native.Owner || p.BranchID != p.Native.BranchID || !functionResourceUID.MatchString(p.BootstrapCredentialUID) || len(p.BootstrapVerifierSHA256) != 64 || len(p.ProbeVerifierSHA256) != 64 || !functionResourceUID.MatchString(p.RoutesUID) || len(p.ConfigUIDs) == 0 || len(p.ConfigUIDs) > 128 || p.ConfigUIDs[p.EndpointID] == "" || len(p.RoleHashes) != len(p.ConfigUIDs) || len(p.ColdOwners) != len(p.ConfigUIDs) {
		return false
	}
	for id, uid := range p.ConfigUIDs {
		owner, known := p.ColdOwners[id]
		if !functionWriterEndpointID.MatchString(id) || !functionResourceUID.MatchString(uid) || len(p.RoleHashes[id]) != 64 || !known || owner != "" && !validPGIdentifier(owner) {
			return false
		}
	}
	return p.Mode == "platform" && p.Native.Platform && p.TargetRoleVersion == 0 && p.TargetCredentialRef == "" && p.TargetCredentialUID == "" && p.TargetVerifierSHA256 == "" || p.Mode == "managed" && !p.Native.Platform && p.TargetRoleVersion > 0 && p.TargetCredentialRef != "" && functionResourceUID.MatchString(p.TargetCredentialUID) && len(p.TargetVerifierSHA256) == 64
}
func (p defaultOwnerPayload) credentialScope(operation string) defaultOwnerCredentialScope {
	return defaultOwnerCredentialScope{OperationID: operation, ProjectID: p.ProjectID, BranchID: p.BranchID, EndpointID: p.EndpointID, CredentialUID: p.BootstrapCredentialUID, Native: p.Native.Scope, DatabaseOID: p.DatabaseOID}
}

func readDefaultOwnerContext(ctx context.Context, db connectionDefaultReader, project, branch string) (defaultOwnerContext, error) {
	value := defaultOwnerContext{ProjectID: project, BranchID: branch}
	err := db.QueryRow(ctx, `SELECT e.id,e.role_name,e.database_name,p.tenant_id,b.timeline_id FROM projects p JOIN branches b ON b.project_id=p.id JOIN organizations o ON o.id=p.org_id
 JOIN endpoints e ON e.project_id=p.id AND e.branch_id=b.id AND e.endpoint_type='read_write' AND e.state='active' AND e.deleted_at IS NULL
 WHERE p.id=$1 AND b.id=$2 AND p.source='managed' AND p.state='ready' AND p.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL AND o.state='active'`, project, branch).Scan(&value.EndpointID, &value.PlatformRole, &value.Database, &value.Scope.TenantID, &value.Scope.TimelineID)
	if err != nil {
		return value, err
	}
	if value.PlatformRole != "cloud_admin" || !value.Scope.valid() || !validPGIdentifier(value.Database) || protectedPGDatabase(value.Database) && value.Database != "postgres" {
		return value, renameFault(409, "default_owner_platform_identity_rejected", "Original managed platform role and database required")
	}
	return value, nil
}
func readDefaultOwnerBinding(ctx context.Context, db connectionDefaultReader, project, branch string) (*defaultOwnerBinding, error) {
	var b defaultOwnerBinding
	var raw []byte
	err := db.QueryRow(ctx, `SELECT database_name,database_oid,tenant_id,timeline_id,platform_role,platform_role_oid,initial_owner,initial_owner_oid,comment_sha256,database_flags,mode,owner_name,owner_oid,owner_role_version,owner_credential_ref,owner_credential_uid,version,state,COALESCE(current_operation_id,''),COALESCE(last_operation_id,''),basis FROM default_database_owners WHERE project_id=$1 AND branch_id=$2`, project, branch).Scan(&b.Database, &b.DatabaseOID, &b.Scope.TenantID, &b.Scope.TimelineID, &b.PlatformRole, &b.PlatformRoleOID, &b.InitialOwner, &b.InitialOwnerOID, &b.CommentSHA256, &raw, &b.Mode, &b.Owner, &b.OwnerOID, &b.RoleVersion, &b.CredentialRef, &b.CredentialUID, &b.Version, &b.State, &b.OperationID, &b.LastOperationID, &b.Basis)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &b.Flags); err != nil {
		return nil, err
	}
	return &b, nil
}

// A fixed maintenance database keeps the original default observable even if
// an external session disabled connections to that database. No caller target
// or alternate host is accepted.
func (s *server) defaultOwnerConnection(ctx context.Context, c defaultOwnerContext, password string, p *defaultOwnerPayload) (*pgx.Conn, string, string, error) {
	secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", kubeName(c.EndpointID)+"-credentials"), nil)
	if err != nil || !owned(secret, c.ProjectID, c.EndpointID) {
		return nil, "", "", errors.New("original default Owner endpoint credential unavailable")
	}
	uid := stringVal(nested(secret, "metadata", "uid"))
	if !functionResourceUID.MatchString(uid) {
		return nil, "", "", errors.New("original default Owner credential UID required")
	}
	verifier, err := secretText(secret, "adminVerifier")
	if err != nil {
		return nil, "", "", err
	}
	digest := catalogMarkerDigest(verifier)
	if p != nil && (p.BootstrapCredentialUID != uid || p.BootstrapVerifierSHA256 != digest || p.EndpointID != c.EndpointID || p.Native.Scope != c.Scope || p.Name != c.Database || p.Native.PlatformRole != c.PlatformRole) {
		return nil, "", "", errors.New("original default Owner credential intent changed")
	}
	if p != nil {
		probeVerifier, e := secretText(secret, "probeVerifier")
		if e != nil || catalogMarkerDigest(probeVerifier) != p.ProbeVerifierSHA256 {
			return nil, "", "", errors.New("original service probe credential intent changed")
		}
	}
	role := c.PlatformRole
	if password == "" {
		role = "control_probe"
		password, err = secretText(secret, "probePassword")
		if err != nil {
			return nil, "", "", err
		}
	}
	conn, err := connectSQL(ctx, s.proxyHost, s.proxyPort, role, password, "template1", selector(c.EndpointID))
	return conn, uid, digest, err
}
func (s *server) observeDefaultOwner(ctx context.Context, c defaultOwnerContext) (catalogNativeSnapshot, error) {
	release, err := s.acquireProbeGate(ctx, c.EndpointID, true)
	if err != nil {
		return catalogNativeSnapshot{}, err
	}
	defer release()
	conn, _, _, err := s.defaultOwnerConnection(ctx, c, "", nil)
	if err != nil {
		return catalogNativeSnapshot{}, err
	}
	defer conn.Close(context.Background())
	return readCatalogNativeSnapshot(ctx, conn, c.Scope)
}
func defaultOwnerNativeRows(snapshot catalogNativeSnapshot, c defaultOwnerContext) (catalogNativeDatabase, catalogNativeRole, error) {
	var database catalogNativeDatabase
	var platform catalogNativeRole
	for _, d := range snapshot.Databases {
		if d.Name == c.Database {
			database = d
		}
	}
	for _, r := range snapshot.Roles {
		if r.Name == c.PlatformRole {
			platform = r
		}
	}
	if database.OID == 0 || platform.OID == 0 || !platform.Flags.Superuser || !platform.Flags.Login {
		return database, platform, renameFault(409, "default_owner_native_identity_rejected", "Existing original default database and platform identity required")
	}
	if database.Flags.ConnectionLimit < -1 {
		return database, platform, renameFault(409, "default_database_unavailable", "Original default database is invalid; preserve it for recovery")
	}
	return database, platform, nil
}
func checkDefaultOwnerKnownIdentity(ctx context.Context, db connectionDefaultReader, project, branch, kind, name string, oid uint32, scope catalogNativeScope) error {
	// Known history cannot be overwritten by a same-name replacement. An
	// authorized metadata rename can retain the same original physical OID.
	var conflict bool
	err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM catalog_identity_baselines WHERE project_id=$1 AND branch_id=$2 AND kind=$3 AND ((name=$4 AND object_oid<>$5) OR (object_oid=$5 AND (tenant_id<>$6 OR timeline_id<>$7))))`, project, branch, kind, name, int64(oid), scope.TenantID, scope.TimelineID).Scan(&conflict)
	if err != nil {
		return err
	}
	if conflict {
		return renameFault(409, "catalog_identity_conflict", "Recorded original object differs from the current native identity")
	}
	return nil
}
func (s *server) defaultOwnerRole(ctx context.Context, c defaultOwnerContext, input defaultOwnerInput, snapshot catalogNativeSnapshot) (defaultOwnerRoleProof, error) {
	proof := defaultOwnerRoleProof{Name: input.Owner, OID: input.OwnerOID}
	if err := checkCatalogForkExclusion(ctx, s.db, c.ProjectID, c.BranchID, "role", input.Owner); err != nil {
		return proof, err
	}
	err := s.db.QueryRow(ctx, `SELECT version,credential_ref FROM branch_roles WHERE branch_id=$1 AND name=$2 AND state='ready'`, c.BranchID, input.Owner).Scan(&proof.Version, &proof.CredentialRef)
	if isNoRows(err) {
		return proof, renameFault(409, "managed_owner_required", "Ready API-managed Owner required")
	}
	if err != nil {
		return proof, err
	}
	canonical := catalogRoleFlags{Login: true, Inherit: true, CreateRole: true, CreateDB: true, Replication: true, BypassRLS: true, ConnectionLimit: -1}
	found := false
	for _, r := range snapshot.Roles {
		if r.Name == proof.Name && r.OID == proof.OID && r.Flags == canonical && r.Marker == "neon-control/branch/"+c.BranchID {
			proof.Flags = r.Flags
			proof.MarkerSHA256 = catalogMarkerDigest(r.Marker)
			found = true
		}
	}
	if !found {
		return proof, renameFault(409, "managed_owner_identity_conflict", "Original managed Owner OID, options and branch marker required")
	}
	if err = checkDefaultOwnerKnownIdentity(ctx, s.db, c.ProjectID, c.BranchID, "role", proof.Name, proof.OID, c.Scope); err != nil {
		return proof, err
	}
	secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", proof.CredentialRef), nil)
	if err != nil || nested(secret, "metadata", "labels", "neon-control/project-id") != c.ProjectID || secret["immutable"] != true {
		return proof, errors.New("original managed Owner credential identity unavailable")
	}
	proof.CredentialUID = stringVal(nested(secret, "metadata", "uid"))
	if !functionResourceUID.MatchString(proof.CredentialUID) {
		return proof, errors.New("original managed Owner credential UID required")
	}
	verifier, e := secretText(secret, "verifier")
	if e != nil || !functionsql.ValidVerifier(verifier) {
		return proof, errors.New("original managed Owner verifier intent unavailable")
	}
	proof.VerifierSHA256 = catalogMarkerDigest(verifier)
	var replaced bool
	err = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM catalog_identity_baselines WHERE project_id=$1 AND branch_id=$2 AND kind='role' AND object_oid=$3 AND credential_ref=$4 AND credential_uid<>$5)`, c.ProjectID, c.BranchID, int64(proof.OID), proof.CredentialRef, proof.CredentialUID).Scan(&replaced)
	if err != nil {
		return proof, err
	}
	if replaced {
		return proof, renameFault(409, "catalog_credential_identity_conflict", "Recorded immutable Owner credential was replaced")
	}
	return proof, nil
}

func (s *server) defaultDatabaseOwnerRead(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), sqlTotalTimeout)
	defer cancel()
	c, err := readDefaultOwnerContext(ctx, s.db, r.PathValue("project"), r.PathValue("branch"))
	if err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	b, err := readDefaultOwnerBinding(ctx, s.db, c.ProjectID, c.BranchID)
	if err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	snapshot, err := s.observeDefaultOwner(ctx, c)
	if err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	database, platform, err := defaultOwnerNativeRows(snapshot, c)
	if err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	owner := ""
	for _, v := range snapshot.Roles {
		if v.OID == database.OwnerOID {
			owner = v.Name
		}
	}
	mode, state, status, version := "uninitialized", "uninitialized", "initial_observation_required", int64(1)
	value := record{"database": c.Database, "database_oid": database.OID, "observed_owner": owner, "observed_owner_oid": database.OwnerOID, "platform_role": c.PlatformRole, "platform_role_oid": platform.OID, "branch_id": c.BranchID, "endpoint_id": c.EndpointID, "history_before_initial_observation": "not_proven"}
	if b != nil {
		mode, state, version = b.Mode, b.State, b.Version
		status = "owner_matches"
		switch {
		case b.Scope != c.Scope || b.Database != c.Database:
			status = "scope_conflict"
		case b.DatabaseOID != database.OID:
			status = "replaced"
		case b.PlatformRoleOID != platform.OID || b.PlatformRole != platform.Name:
			status = "platform_identity_conflict"
		case state == "updating":
			status = "operation_in_progress"
		case b.OwnerOID != database.OwnerOID:
			status = "owner_changed"
		case b.Owner != owner:
			status = "owner_role_renamed"
		}
		value["owner"] = b.Owner
		value["owner_oid"] = b.OwnerOID
		value["last_operation_id"] = b.LastOperationID
		value["operation_id"] = b.OperationID
		value["initial_comment_changed"] = b.CommentSHA256 != catalogMarkerDigest(database.Marker)
		value["initial_options_changed"] = b.Flags != database.Flags
	}
	value["mode"] = mode
	value["state"] = state
	value["status"] = status
	value["version_token"] = strconv.FormatInt(version, 10)
	roles, err := s.many(ctx, `SELECT name,version FROM branch_roles WHERE branch_id=$1 AND state='ready' ORDER BY name LIMIT 501`, c.BranchID)
	if err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	if len(roles) > 500 {
		s.defaultOwnerFailure(w, r, errCatalogInventoryBound)
		return
	}
	candidates := []record{}
	canonical := catalogRoleFlags{Login: true, Inherit: true, CreateRole: true, CreateDB: true, Replication: true, BypassRLS: true, ConnectionLimit: -1}
	for _, role := range roles {
		for _, native := range snapshot.Roles {
			if native.Name == role["name"] && !protectedPGRole(native.Name) && native.Flags == canonical && native.Marker == "neon-control/branch/"+c.BranchID {
				if e := checkCatalogForkExclusion(ctx, s.db, c.ProjectID, c.BranchID, "role", native.Name); e != nil {
					var fault *catalogRenameFault
					if errors.As(e, &fault) {
						continue
					}
					s.defaultOwnerFailure(w, r, e)
					return
				}
				e := checkDefaultOwnerKnownIdentity(ctx, s.db, c.ProjectID, c.BranchID, "role", native.Name, native.OID, c.Scope)
				var fault *catalogRenameFault
				if e != nil {
					if errors.As(e, &fault) {
						continue
					}
					s.defaultOwnerFailure(w, r, e)
					return
				}
				candidates = append(candidates, record{"name": native.Name, "oid": native.OID, "version_token": fmt.Sprint(role["version"])})
			}
		}
	}
	value["managed_owners"] = candidates
	w.Header().Set("ETag", `"`+strconv.FormatInt(version, 10)+`"`)
	jsonResponse(w, 200, value)
}
func (s *server) defaultOwnerFailure(w http.ResponseWriter, r *http.Request, err error) {
	var fault *catalogRenameFault
	if errors.As(err, &fault) {
		fail(w, r, fault.Status, fault.Code, fault.Message)
		return
	}
	if errors.Is(err, errCatalogInventoryScope) {
		fail(w, r, 409, "catalog_scope_conflict", "Native default database scope rejected")
		return
	}
	if errors.Is(err, errCatalogInventoryBound) {
		fail(w, r, 409, "catalog_inventory_limit", "Complete bounded native observation required")
		return
	}
	fail(w, r, 503, "default_database_owner_unavailable", "Could not establish default database Owner; retain the original request key")
}
