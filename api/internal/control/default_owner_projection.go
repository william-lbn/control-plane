package control

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

func (s *server) defaultOwnerCatalogProjection(ctx context.Context, project, branch string) (*defaultOwnerBinding, error) {
	b, err := readDefaultOwnerBinding(ctx, s.db, project, branch)
	if err != nil {
		return nil, err
	}
	if b != nil && b.State != "ready" {
		return nil, renameFault(409, "default_owner_operation_in_progress", "Complete or retry the original default database Owner Operation first")
	}
	return b, nil
}
func copyDefaultOwnerBindingTx(ctx context.Context, tx pgx.Tx, project, parent, child, tenant, timeline, actor string) error {
	if err := checkDefaultOwnerReservation(ctx, tx, project, parent); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO default_database_owners(branch_id,project_id,database_name,database_oid,tenant_id,timeline_id,platform_role,platform_role_oid,
 initial_owner,initial_owner_oid,comment_sha256,database_flags,mode,owner_name,owner_oid,owner_role_version,owner_credential_ref,owner_credential_uid,version,state,basis,updated_by)
 SELECT $3,project_id,database_name,database_oid,$4,$5,platform_role,platform_role_oid,initial_owner,initial_owner_oid,comment_sha256,database_flags,
 mode,owner_name,owner_oid,owner_role_version,owner_credential_ref,owner_credential_uid,2,'ready','live_fork',$6
 FROM default_database_owners WHERE project_id=$1 AND branch_id=$2 AND state='ready'`, project, parent, child, tenant, timeline, actor)
	return err
}
func (s *server) verifyInheritedDefaultOwner(ctx context.Context, p createPayload, b *defaultOwnerBinding) error {
	if b == nil {
		return nil
	}
	if b.Scope != (catalogNativeScope{TenantID: p.TenantID, TimelineID: p.TimelineID}) {
		return errors.New("inherited original default database scope rejected")
	}
	c := defaultOwnerContext{ProjectID: p.ProjectID, BranchID: p.BranchID, EndpointID: p.EndpointID, PlatformRole: b.PlatformRole, Database: b.Database, Scope: b.Scope}
	conn, _, _, err := s.defaultOwnerConnection(ctx, c, "", nil)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	snapshot, err := readCatalogNativeSnapshot(ctx, conn, c.Scope)
	if err != nil {
		return err
	}
	database, platform, err := defaultOwnerNativeRows(snapshot, c)
	if err != nil {
		return err
	}
	if database.OID != b.DatabaseOID || database.OwnerOID != b.OwnerOID || platform.OID != b.PlatformRoleOID {
		return errors.New("inherited original default database or Owner OID changed")
	}
	found := false
	for _, role := range snapshot.Roles {
		if role.OID == b.OwnerOID && role.Name == b.Owner {
			found = true
		}
	}
	if !found {
		return errors.New("inherited original default database Owner name changed")
	}
	// Its arbitrary database comment is user content and is never rewritten by
	// catalog adoption. Current native OID/Owner and the new fork scope bind it.
	return nil
}
