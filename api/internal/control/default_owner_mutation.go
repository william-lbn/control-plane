package control

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (s *server) defaultDatabaseOwnerMutation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var input defaultOwnerInput
	if readJSON(r, &input) != nil || !input.valid() {
		fail(w, r, 422, "invalid_default_database_owner", "Acknowledge the observed original default database and choose a managed Owner or original platform Owner with the current platform password")
		return
	}
	header := r.Header.Get("If-Match")
	version, err := strconv.ParseInt(strings.Trim(header, `"`), 10, 64)
	if header == "" {
		fail(w, r, 428, "version_required", "Original quoted default database Owner version required")
		return
	}
	if err != nil || version < 1 || version >= 9223372036854775806 || header != `"`+strconv.FormatInt(version, 10)+`"` {
		fail(w, r, 422, "invalid_version", "Canonical quoted positive version required")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	hash := s.createRequestHash(r.Method+":"+r.URL.Path, record{"input": input, "version": version})
	// The original password is used only in the keyed request digest and AEAD
	// escrow, never a payload, response, SQL literal or diagnostic.
	if s.replayCatalog(w, r, key, hash) {
		return
	}
	project, branch := r.PathValue("project"), r.PathValue("branch")
	ctx, cancel := context.WithTimeout(r.Context(), sqlTotalTimeout)
	defer cancel()
	if err = checkDefaultOwnerReservation(ctx, s.db, project, branch); err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	b, err := readDefaultOwnerBinding(ctx, s.db, project, branch)
	if err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	current := int64(1)
	if b != nil {
		current = b.Version
	}
	if current != version {
		fail(w, r, 412, "version_conflict", "Default database Owner changed; refresh before retrying")
		return
	}
	if input.Initial != (b == nil) {
		fail(w, r, 422, "initial_observation_acknowledgement_required", "Acknowledge the first native identity observation only when no original binding exists")
		return
	}
	var pending bool
	err = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM branch_roles WHERE branch_id=$1 AND state IN ('creating','updating','deleting')) OR EXISTS(SELECT 1 FROM branch_databases WHERE branch_id=$1 AND state IN ('creating','updating','deleting')) OR EXISTS(SELECT 1 FROM operations WHERE project_id=$2 AND state IN ('queued','running','retry_wait') AND (resource_id=$1 OR payload->>'branch_id'=$1))`, branch, project).Scan(&pending)
	if err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	if pending {
		fail(w, r, 409, "branch_operation_in_progress", "Complete the original branch catalog Operation before changing default Owner")
		return
	}
	c, err := readDefaultOwnerContext(ctx, s.db, project, branch)
	if err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	release, err := s.acquireProbeGate(ctx, c.EndpointID, true)
	if err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	defer release()
	conn, credentialUID, verifierDigest, err := s.defaultOwnerConnection(ctx, c, input.Password, nil)
	if err != nil {
		s.sqlFailureResponse(w, r, err)
		return
	}
	defer conn.Close(context.Background())
	snapshot, err := readCatalogNativeSnapshot(ctx, conn, c.Scope)
	if err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	database, platform, err := defaultOwnerNativeRows(snapshot, c)
	if err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	previousOwner := ""
	for _, role := range snapshot.Roles {
		if role.OID == database.OwnerOID {
			previousOwner = role.Name
		}
	}
	if database.OID != input.DatabaseOID || database.OwnerOID != input.PreviousOwnerOID {
		fail(w, r, 409, "default_owner_observation_changed", "Observed default database or previous Owner OID changed")
		return
	}
	if b != nil && (b.Scope != c.Scope || b.Database != c.Database || b.DatabaseOID != database.OID || b.PlatformRoleOID != platform.OID || b.PlatformRole != c.PlatformRole || b.OwnerOID != database.OwnerOID || b.Owner != previousOwner) {
		fail(w, r, 409, "default_owner_identity_conflict", "Recorded original default database, platform identity or Owner differs from native state")
		return
	}
	if err = checkDefaultOwnerKnownIdentity(ctx, s.db, project, branch, "database", c.Database, database.OID, c.Scope); err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	p := defaultOwnerPayload{catalogPayload: catalogPayload{ProjectID: project, BranchID: branch, EndpointID: c.EndpointID, Name: c.Database, PreviousOwner: previousOwner, DatabaseOID: database.OID, ExpectedVersion: version}, Mode: input.Mode, Initial: input.Initial, BootstrapCredentialUID: credentialUID, BootstrapVerifierSHA256: verifierDigest,
		Native: defaultOwnerSQLIntent{Scope: c.Scope, Database: c.Database, PlatformRole: c.PlatformRole, PreviousOwner: previousOwner, PreviousOwnerOID: database.OwnerOID, DatabaseOID: database.OID, PlatformRoleOID: platform.OID, CommentSHA256: catalogMarkerDigest(database.Marker), Flags: database.Flags, BranchID: branch}}
	if input.Mode == "platform" {
		p.Owner = c.PlatformRole
		p.Native.Owner = c.PlatformRole
		p.Native.OwnerOID = platform.OID
		p.Native.Platform = true
	} else {
		proof, e := s.defaultOwnerRole(ctx, c, input, snapshot)
		if e != nil {
			s.defaultOwnerFailure(w, r, e)
			return
		}
		p.Owner = proof.Name
		p.TargetRoleVersion = proof.Version
		p.TargetCredentialRef = proof.CredentialRef
		p.TargetCredentialUID = proof.CredentialUID
		p.TargetVerifierSHA256 = proof.VerifierSHA256
		p.Native.Owner = proof.Name
		p.Native.OwnerOID = proof.OID
		p.Native.TargetRoleFlags = proof.Flags
		p.Native.TargetMarkerSHA256 = proof.MarkerSHA256
	}
	if b != nil && p.Native.OwnerOID == b.OwnerOID {
		fail(w, r, 422, "owner_unchanged", "Choose a different default database Owner")
		return
	}
	txNative, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	if _, err = txNative.Exec(ctx, `SET LOCAL TIME ZONE 'UTC'`); err == nil {
		var observed uint32
		observed, p.Native.PreviousACL, err = readDefaultOwnerDatabaseTx(ctx, txNative, p.Native)
		if err == nil && observed != p.Native.PreviousOwnerOID {
			err = renameFault(409, "default_owner_observation_changed", "Previous Owner changed before admission")
		}
	}
	if err == nil {
		err = verifyDefaultOwnerTargetTx(ctx, txNative, p.Native)
	}
	_ = txNative.Rollback(context.Background())
	if err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	credential, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", kubeName(c.EndpointID)+"-credentials"), nil)
	if err != nil || !owned(credential, project, c.EndpointID) || stringVal(nested(credential, "metadata", "uid")) != credentialUID {
		s.defaultOwnerFailure(w, r, errors.New("original platform credential identity changed"))
		return
	}
	probeVerifier, err := secretText(credential, "probeVerifier")
	if err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	p.ProbeVerifierSHA256 = catalogMarkerDigest(probeVerifier)
	var legacyState, legacyOwner string
	err = s.db.QueryRow(ctx, `SELECT state,owner_name,version FROM branch_databases WHERE branch_id=$1 AND name=$2`, branch, p.Name).Scan(&legacyState, &legacyOwner, &p.LegacyDatabaseVersion)
	if err != nil && !isNoRows(err) {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	if err == nil && (legacyState != "ready" || legacyOwner != p.PreviousOwner) {
		fail(w, r, 409, "default_owner_intent_conflict", "Original default database custom-catalog intent differs from native Owner")
		return
	}
	if err = s.captureDefaultOwnerBackings(ctx, &p); err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	operation := newID("op_")
	keyVersion, sealed, err := s.sealDefaultOwnerPassword(p.credentialScope(operation), input.Password)
	if err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.defaultOwnerFailure(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	err = admitDefaultOwnerTx(ctx, tx, p, operation, userFrom(r).ID, requestID(r), keyHash(key), hash, keyVersion, sealed)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		if s.replayCatalog(w, r, key, hash) {
			return
		}
		s.defaultOwnerFailure(w, r, err)
		return
	}
	s.acceptCatalog(w, r, operation)
}
