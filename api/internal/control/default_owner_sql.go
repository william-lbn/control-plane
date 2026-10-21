package control

import (
	"context"
	"errors"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
)

type defaultOwnerACL struct {
	Grantor   uint32 `json:"grantor"`
	Grantee   uint32 `json:"grantee"`
	Privilege string `json:"privilege"`
	Grantable bool   `json:"grantable"`
}
type defaultOwnerSQLIntent struct {
	Scope                                                    catalogNativeScope
	Database, PlatformRole, PreviousOwner, Owner, BranchID   string
	DatabaseOID, PlatformRoleOID, PreviousOwnerOID, OwnerOID uint32
	CommentSHA256                                            string
	Flags                                                    catalogDatabaseFlags
	PreviousACL                                              []defaultOwnerACL
	TargetRoleFlags                                          catalogRoleFlags
	TargetMarkerSHA256                                       string
	Platform                                                 bool
}

func canonicalDefaultOwnerACL(rows []defaultOwnerACL) ([]defaultOwnerACL, error) {
	if len(rows) > catalogInventoryLimit {
		return nil, errCatalogInventoryBound
	}
	byKey := map[string]defaultOwnerACL{}
	for _, row := range rows {
		if row.Grantor == 0 || (row.Privilege != "CREATE" && row.Privilege != "CONNECT" && row.Privilege != "TEMPORARY") {
			return nil, errors.New("default database ACL rejected")
		}
		key := strconv.FormatUint(uint64(row.Grantor), 10) + ":" + strconv.FormatUint(uint64(row.Grantee), 10) + ":" + row.Privilege
		if old, exists := byKey[key]; exists {
			row.Grantable = row.Grantable || old.Grantable
		}
		byKey[key] = row
	}
	result := make([]defaultOwnerACL, 0, len(byKey))
	for _, row := range byKey {
		result = append(result, row)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Grantor != b.Grantor {
			return a.Grantor < b.Grantor
		}
		if a.Grantee != b.Grantee {
			return a.Grantee < b.Grantee
		}
		return a.Privilege < b.Privilege
	})
	return result, nil
}
func defaultOwnerACLAfter(rows []defaultOwnerACL, oldOwner, newOwner uint32) ([]defaultOwnerACL, error) {
	if oldOwner == 0 || newOwner == 0 {
		return nil, errors.New("default Owner ACL identity missing")
	}
	result := append([]defaultOwnerACL{}, rows...)
	for i := range result {
		if result[i].Grantor == oldOwner {
			result[i].Grantor = newOwner
		}
		if result[i].Grantee == oldOwner {
			result[i].Grantee = newOwner
		}
	}
	return canonicalDefaultOwnerACL(result)
}
func equalDefaultOwnerACL(a, b []defaultOwnerACL) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func readDefaultOwnerACL(ctx context.Context, tx pgx.Tx, oid uint32) ([]defaultOwnerACL, error) {
	rows, err := tx.Query(ctx, `SELECT a.grantor,a.grantee,a.privilege_type,a.is_grantable FROM pg_database d CROSS JOIN LATERAL aclexplode(COALESCE(d.datacl,acldefault('d',d.datdba))) a WHERE d.oid=$1 ORDER BY a.grantor,a.grantee,a.privilege_type LIMIT 1025`, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []defaultOwnerACL{}
	for rows.Next() {
		var row defaultOwnerACL
		if err = rows.Scan(&row.Grantor, &row.Grantee, &row.Privilege, &row.Grantable); err != nil {
			return nil, err
		}
		if len(result) == catalogInventoryLimit {
			return nil, errCatalogInventoryBound
		}
		result = append(result, row)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return canonicalDefaultOwnerACL(result)
}
func (v defaultOwnerSQLIntent) valid() bool {
	return v.Scope.valid() && defaultOwnerBranchID.MatchString(v.BranchID) && v.DatabaseOID > 0 && v.PlatformRole == "cloud_admin" && v.PlatformRoleOID > 0 && v.PreviousOwnerOID > 0 && v.OwnerOID > 0 && v.Flags.ConnectionLimit >= -1 && validPGIdentifier(v.Database) && (!protectedPGDatabase(v.Database) || v.Database == "postgres") && validPGIdentifier(v.PreviousOwner) && validPGIdentifier(v.Owner) && len(v.CommentSHA256) == 64 && (v.PreviousOwnerOID != v.OwnerOID || v.PreviousOwner == v.Owner) && (!v.Platform && !protectedPGRole(v.Owner) || v.Platform && v.Owner == v.PlatformRole && v.OwnerOID == v.PlatformRoleOID)
}
func readDefaultOwnerDatabaseTx(ctx context.Context, tx pgx.Tx, p defaultOwnerSQLIntent) (uint32, []defaultOwnerACL, error) {
	return readDefaultOwnerDatabaseSnapshotTx(ctx, tx, p, true)
}
func readDefaultOwnerDatabaseSnapshotTx(ctx context.Context, tx pgx.Tx, p defaultOwnerSQLIntent, requirePlatformSession bool) (uint32, []defaultOwnerACL, error) {
	var scope catalogNativeScope
	var oid, owner, platformOID uint32
	var ownerName, comment, session string
	var flags catalogDatabaseFlags
	var superuser bool
	err := tx.QueryRow(ctx, `SELECT current_setting('neon.tenant_id',true),current_setting('neon.timeline_id',true),current_user`).Scan(&scope.TenantID, &scope.TimelineID, &session)
	if err != nil || scope != p.Scope {
		return 0, nil, errCatalogInventoryScope
	}
	if requirePlatformSession && session != p.PlatformRole {
		return 0, nil, errors.New("original platform session identity rejected")
	}
	err = tx.QueryRow(ctx, `SELECT oid,rolsuper FROM pg_roles WHERE rolname=$1`, p.PlatformRole).Scan(&platformOID, &superuser)
	if err != nil || platformOID != p.PlatformRoleOID || !superuser {
		return 0, nil, errors.New("original platform role identity rejected")
	}
	err = tx.QueryRow(ctx, `SELECT oid,datdba,pg_get_userbyid(datdba),COALESCE(left(shobj_description(oid,'pg_database'),4097),''),datallowconn,datistemplate,datconnlimit FROM pg_database WHERE datname=$1`, p.Database).Scan(&oid, &owner, &ownerName, &comment, &flags.AllowConnections, &flags.Template, &flags.ConnectionLimit)
	if err != nil || oid != p.DatabaseOID || len(comment) > 4096 || catalogMarkerDigest(comment) != p.CommentSHA256 || flags != p.Flags || (owner != p.PreviousOwnerOID && owner != p.OwnerOID) || (owner == p.PreviousOwnerOID && ownerName != p.PreviousOwner) || (owner == p.OwnerOID && ownerName != p.Owner) {
		return 0, nil, errors.New("original default database OID, comment, options or owner rejected")
	}
	acl, err := readDefaultOwnerACL(ctx, tx, oid)
	return owner, acl, err
}

// Observation can prove an already achieved original postcondition without
// opening the password escrow again. It never performs privileged SQL.
func observeDefaultOwnerSQL(ctx context.Context, conn *pgx.Conn, p defaultOwnerSQLIntent, requireTarget bool) (bool, error) {
	if !p.valid() {
		return false, errors.New("invalid original default Owner observation")
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SET LOCAL statement_timeout='10s'; SET LOCAL lock_timeout='2s'; SET LOCAL TIME ZONE 'UTC'`, pgx.QueryExecModeSimpleProtocol); err != nil {
		return false, err
	}
	owner, acl, err := readDefaultOwnerDatabaseSnapshotTx(ctx, tx, p, false)
	if err != nil {
		return false, err
	}
	if err = verifyDefaultOwnerTargetTx(ctx, tx, p); err != nil {
		return false, err
	}
	expected, err := canonicalDefaultOwnerACL(p.PreviousACL)
	if err != nil {
		return false, err
	}
	if owner == p.OwnerOID {
		expected, err = defaultOwnerACLAfter(expected, p.PreviousOwnerOID, p.OwnerOID)
		if err != nil {
			return false, err
		}
	}
	if !equalDefaultOwnerACL(acl, expected) || requireTarget && owner != p.OwnerOID {
		return false, errors.New("original default database Owner/ACL observation changed")
	}
	return owner == p.OwnerOID, tx.Commit(ctx)
}
func verifyDefaultOwnerTargetTx(ctx context.Context, tx pgx.Tx, p defaultOwnerSQLIntent) error {
	var oid uint32
	var marker string
	var f catalogRoleFlags
	err := tx.QueryRow(ctx, `SELECT oid,COALESCE(left(shobj_description(oid,'pg_authid'),4097),''),rolcanlogin,rolsuper,rolinherit,rolcreaterole,rolcreatedb,rolreplication,rolbypassrls,rolconnlimit,COALESCE(rolvaliduntil::text,'') FROM pg_roles WHERE rolname=$1`, p.Owner).Scan(&oid, &marker, &f.Login, &f.Superuser, &f.Inherit, &f.CreateRole, &f.CreateDB, &f.Replication, &f.BypassRLS, &f.ConnectionLimit, &f.ValidUntil)
	if err != nil || oid != p.OwnerOID || len(marker) > 4096 {
		return errors.New("default database target role identity rejected")
	}
	if p.Platform {
		if !f.Superuser || oid != p.PlatformRoleOID {
			return errors.New("original platform Owner rejected")
		}
		return nil
	}
	if !f.Login || f.Superuser || f != p.TargetRoleFlags || catalogMarkerDigest(marker) != p.TargetMarkerSHA256 || marker != "neon-control/branch/"+p.BranchID {
		return errors.New("original managed target Owner options or marker rejected")
	}
	return nil
}
func applyDefaultDatabaseOwnerSQL(ctx context.Context, conn *pgx.Conn, p defaultOwnerSQLIntent) error {
	if !p.valid() {
		return errors.New("invalid default database Owner intent")
	}
	previousACL, err := canonicalDefaultOwnerACL(p.PreviousACL)
	if err != nil {
		return err
	}
	targetACL, err := defaultOwnerACLAfter(previousACL, p.PreviousOwnerOID, p.OwnerOID)
	if err != nil {
		return err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout='5s'; SET LOCAL statement_timeout='30s'; SET LOCAL TIME ZONE 'UTC'`, pgx.QueryExecModeSimpleProtocol); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,794210064))`, p.BranchID); err != nil {
		return err
	}
	current, acl, err := readDefaultOwnerDatabaseTx(ctx, tx, p)
	if err != nil {
		return err
	}
	if err = verifyDefaultOwnerTargetTx(ctx, tx, p); err != nil {
		return err
	}
	if current == p.PreviousOwnerOID {
		if !equalDefaultOwnerACL(acl, previousACL) {
			return errors.New("original default database ACL changed")
		}
	} else if !equalDefaultOwnerACL(acl, targetACL) {
		return errors.New("restored default database ACL changed")
	}
	if current != p.OwnerOID {
		if _, err = tx.Exec(ctx, "ALTER DATABASE "+pgQuote(p.Database)+" OWNER TO "+pgQuote(p.Owner)); err != nil {
			return sqlExecutionError{"default_database_owner", err}
		}
	}
	current, acl, err = readDefaultOwnerDatabaseTx(ctx, tx, p)
	if err != nil || current != p.OwnerOID || !equalDefaultOwnerACL(acl, targetACL) {
		return errors.New("default database Owner postcondition rejected; transaction rolled back")
	}
	if err = verifyDefaultOwnerTargetTx(ctx, tx, p); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
