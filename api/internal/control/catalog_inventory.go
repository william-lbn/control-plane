package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const catalogInventoryLimit = 1024

var errCatalogInventoryScope = errors.New("native catalog tenant/timeline rejected")
var errCatalogInventoryBound = errors.New("native catalog observation exceeded its bound")

type catalogNativeScope struct{ TenantID, TimelineID string }

func (s catalogNativeScope) valid() bool {
	for _, v := range []string{s.TenantID, s.TimelineID} {
		if len(v) != 32 || strings.Trim(v, "0123456789abcdef") != "" {
			return false
		}
	}
	return true
}

type catalogRoleFlags struct {
	Login, Superuser, Inherit, CreateRole, CreateDB, Replication, BypassRLS bool
	ConnectionLimit                                                         int32
	ValidUntil                                                              string
}
type catalogDatabaseFlags struct {
	AllowConnections, Template bool
	ConnectionLimit            int32
}
type catalogNativeRole struct {
	OID    uint32
	Name   string
	Marker string `json:"-"`
	Flags  catalogRoleFlags
}
type catalogNativeDatabase struct {
	OID, OwnerOID uint32
	Name          string
	Marker        string `json:"-"`
	Flags         catalogDatabaseFlags
}

// Internal snapshot only. Password/verifier columns and user tables are never
// selected. Marker text may be user supplied, so it is excluded from JSON.
type catalogNativeSnapshot struct {
	Scope     catalogNativeScope
	Roles     []catalogNativeRole
	Databases []catalogNativeDatabase
}
type catalogIdentityExpectation struct {
	Scope                 catalogNativeScope
	Kind, Name, OwnerName string
	OID, OwnerOID         uint32
	MarkerSHA256          string `json:"-"`
	RoleFlags             catalogRoleFlags
	DatabaseFlags         catalogDatabaseFlags
}
type catalogIdentityDifference struct {
	Kind          string   `json:"kind"`
	ExpectedName  string   `json:"expected_name"`
	ExpectedOID   uint32   `json:"expected_oid"`
	ObservedName  string   `json:"observed_name,omitempty"`
	ObservedOID   uint32   `json:"observed_oid,omitempty"`
	ObservedOwner string   `json:"observed_owner,omitempty"`
	Status        string   `json:"status"`
	Changes       []string `json:"changes"`
	Credentials   string   `json:"credentials"`
}

func catalogMarkerDigest(v string) string {
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:])
}

func readCatalogNativeSnapshot(ctx context.Context, conn *pgx.Conn, scope catalogNativeScope) (catalogNativeSnapshot, error) {
	result := catalogNativeSnapshot{Scope: scope, Roles: []catalogNativeRole{}, Databases: []catalogNativeDatabase{}}
	if !scope.valid() {
		return result, errCatalogInventoryScope
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SET LOCAL statement_timeout='10s'; SET LOCAL lock_timeout='2s'; SET LOCAL TIME ZONE 'UTC'`, pgx.QueryExecModeSimpleProtocol); err != nil {
		return result, err
	}
	var actual catalogNativeScope
	err = tx.QueryRow(ctx, `SELECT current_setting('neon.tenant_id',true),current_setting('neon.timeline_id',true)`).Scan(&actual.TenantID, &actual.TimelineID)
	if err != nil || actual != scope {
		return result, errCatalogInventoryScope
	}
	roles, err := tx.Query(ctx, `SELECT oid,rolname,COALESCE(left(shobj_description(oid,'pg_authid'),4097),''),
 rolcanlogin,rolsuper,rolinherit,rolcreaterole,rolcreatedb,rolreplication,rolbypassrls,rolconnlimit,COALESCE(rolvaliduntil::text,'')
 FROM pg_roles ORDER BY oid LIMIT 1025`)
	if err != nil {
		return result, err
	}
	for roles.Next() {
		var role catalogNativeRole
		f := &role.Flags
		if err = roles.Scan(&role.OID, &role.Name, &role.Marker, &f.Login, &f.Superuser, &f.Inherit, &f.CreateRole, &f.CreateDB, &f.Replication, &f.BypassRLS, &f.ConnectionLimit, &f.ValidUntil); err != nil {
			roles.Close()
			return result, err
		}
		if len(result.Roles) == catalogInventoryLimit || len(role.Marker) > 4096 {
			roles.Close()
			return result, errCatalogInventoryBound
		}
		result.Roles = append(result.Roles, role)
	}
	err = roles.Err()
	roles.Close()
	if err != nil {
		return result, err
	}
	databases, err := tx.Query(ctx, `SELECT oid,datname,datdba,COALESCE(left(shobj_description(oid,'pg_database'),4097),''),datallowconn,datistemplate,datconnlimit
 FROM pg_database ORDER BY oid LIMIT 1025`)
	if err != nil {
		return result, err
	}
	for databases.Next() {
		var db catalogNativeDatabase
		if err = databases.Scan(&db.OID, &db.Name, &db.OwnerOID, &db.Marker, &db.Flags.AllowConnections, &db.Flags.Template, &db.Flags.ConnectionLimit); err != nil {
			databases.Close()
			return result, err
		}
		if len(result.Databases) == catalogInventoryLimit || len(db.Marker) > 4096 {
			databases.Close()
			return result, errCatalogInventoryBound
		}
		result.Databases = append(result.Databases, db)
	}
	err = databases.Err()
	databases.Close()
	if err != nil {
		return result, err
	}
	if err = validateCatalogNativeSnapshot(result); err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}

func validateCatalogNativeSnapshot(s catalogNativeSnapshot) error {
	if !s.Scope.valid() {
		return errCatalogInventoryScope
	}
	if len(s.Roles) > catalogInventoryLimit || len(s.Databases) > catalogInventoryLimit {
		return errCatalogInventoryBound
	}
	roleOIDs := map[uint32]bool{}
	for _, v := range s.Roles {
		roleOIDs[v.OID] = true
	}
	for _, kind := range []string{"role", "database"} {
		oids := map[uint32]bool{}
		names := map[string]bool{}
		// External SQL can legally create quoted names that the managed CRUD API
		// refuses. Observing those objects must not hide the rest of the catalog.
		check := func(oid uint32, name, marker string) error {
			if oid == 0 || name == "" || len(name) > 63 || !utf8.ValidString(name) || strings.ContainsRune(name, 0) || len(marker) > 4096 || oids[oid] || names[name] {
				return errors.New("native catalog identity observation rejected")
			}
			oids[oid] = true
			names[name] = true
			return nil
		}
		if kind == "role" {
			for _, v := range s.Roles {
				if err := check(v.OID, v.Name, v.Marker); err != nil {
					return err
				}
			}
		} else {
			for _, v := range s.Databases {
				if !roleOIDs[v.OwnerOID] {
					return errors.New("native database owner OID missing from complete observation")
				}
				if err := check(v.OID, v.Name, v.Marker); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Classification confers no repair permission. Original immutable backings,
// current versions/authorization and a fresh native postcondition must be
// established separately before any mutation. Credentials are NOT observed.
func compareCatalogNativeIdentity(expected catalogIdentityExpectation, s catalogNativeSnapshot) catalogIdentityDifference {
	out := catalogIdentityDifference{Kind: expected.Kind, ExpectedName: expected.Name, ExpectedOID: expected.OID, Status: "binding_invalid", Changes: []string{}, Credentials: "not_observed"}
	digest, err := hex.DecodeString(expected.MarkerSHA256)
	if err != nil || len(digest) != 32 || expected.MarkerSHA256 != hex.EncodeToString(digest) || expected.OID == 0 || !validPGIdentifier(expected.Name) || !expected.Scope.valid() || (expected.Kind != "role" && expected.Kind != "database") {
		return out
	}
	if expected.Scope != s.Scope {
		out.Status = "scope_conflict"
		return out
	}
	if validateCatalogNativeSnapshot(s) != nil {
		out.Status = "observation_invalid"
		return out
	}
	var original, nameOccupant uint32
	marker := ""
	roleFlags := catalogRoleFlags{}
	dbFlags := catalogDatabaseFlags{}
	ownerOID := uint32(0)
	if expected.Kind == "role" {
		for _, v := range s.Roles {
			if v.Name == expected.Name {
				nameOccupant = v.OID
			}
			if v.OID == expected.OID {
				original = v.OID
				out.ObservedName = v.Name
				marker = v.Marker
				roleFlags = v.Flags
			}
		}
	} else {
		if expected.OwnerOID == 0 || !validPGIdentifier(expected.OwnerName) {
			return out
		}
		for _, v := range s.Databases {
			if v.Name == expected.Name {
				nameOccupant = v.OID
			}
			if v.OID == expected.OID {
				original = v.OID
				out.ObservedName = v.Name
				marker = v.Marker
				dbFlags = v.Flags
				ownerOID = v.OwnerOID
			}
		}
		for _, v := range s.Roles {
			if v.OID == ownerOID {
				out.ObservedOwner = v.Name
			}
		}
	}
	if original == 0 {
		out.Status = "missing"
		if nameOccupant != 0 {
			out.Status = "replaced"
			out.ObservedOID = nameOccupant
			out.ObservedName = expected.Name
			out.Changes = append(out.Changes, "oid")
		}
		return out
	}
	out.ObservedOID = original
	if out.ObservedName != expected.Name {
		out.Changes = append(out.Changes, "name")
	}
	if catalogMarkerDigest(marker) != expected.MarkerSHA256 {
		out.Changes = append(out.Changes, "provenance")
	}
	if expected.Kind == "role" {
		if roleFlags != expected.RoleFlags {
			out.Changes = append(out.Changes, "role_options")
		}
	} else {
		if ownerOID != expected.OwnerOID {
			out.Changes = append(out.Changes, "owner_oid")
		} else if out.ObservedOwner != expected.OwnerName {
			out.Changes = append(out.Changes, "owner_role_name")
		}
		if dbFlags != expected.DatabaseFlags {
			out.Changes = append(out.Changes, "database_options")
		}
	}
	has := func(v string) bool {
		for _, s := range out.Changes {
			if s == v {
				return true
			}
		}
		return false
	}
	switch {
	case nameOccupant != 0 && nameOccupant != original:
		out.Status = "name_collision"
	case expected.Kind == "role" && roleFlags.Superuser != expected.RoleFlags.Superuser:
		out.Status = "privilege_conflict"
	case has("provenance"):
		out.Status = "provenance_changed"
	case has("owner_oid"):
		out.Status = "owner_changed"
	case has("owner_role_name"):
		out.Status = "owner_role_renamed"
	case has("role_options"):
		out.Status = "role_options_changed"
	case has("database_options"):
		out.Status = "database_options_changed"
	case has("name"):
		out.Status = "renamed"
	default:
		out.Status = "catalog_matches"
	}
	return out
}
