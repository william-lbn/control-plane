package control

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func inventoryFixture() (catalogIdentityExpectation, catalogNativeSnapshot) {
	scope := catalogNativeScope{strings.Repeat("1", 32), strings.Repeat("2", 32)}
	flags := catalogRoleFlags{Login: true, Inherit: true, ConnectionLimit: -1}
	s := catalogNativeSnapshot{Scope: scope, Roles: []catalogNativeRole{
		{OID: 41, Name: "customer", Marker: "managed-marker", Flags: flags},
		{OID: 42, Name: "other_owner", Flags: flags},
	}, Databases: []catalogNativeDatabase{{OID: 51, OwnerOID: 41, Name: "customer_db", Marker: "managed-db", Flags: catalogDatabaseFlags{AllowConnections: true, ConnectionLimit: -1}}}}
	e := catalogIdentityExpectation{Scope: scope, Kind: "role", Name: "customer", OID: 41, MarkerSHA256: catalogMarkerDigest("managed-marker"), RoleFlags: flags}
	return e, s
}

func TestCatalogInventoryClassifiesStableOIDAndConflicts(t *testing.T) {
	cases := []struct {
		name, status, change string
		edit                 func(*catalogIdentityExpectation, *catalogNativeSnapshot)
	}{
		{"same", "catalog_matches", "", func(e *catalogIdentityExpectation, s *catalogNativeSnapshot) {}},
		{"rename", "renamed", "name", func(e *catalogIdentityExpectation, s *catalogNativeSnapshot) { s.Roles[0].Name = "external_name" }},
		{"missing", "missing", "", func(e *catalogIdentityExpectation, s *catalogNativeSnapshot) {
			s.Roles = s.Roles[1:]
			s.Databases = nil
		}},
		{"same-name-replacement", "replaced", "oid", func(e *catalogIdentityExpectation, s *catalogNativeSnapshot) {
			s.Roles[0].OID = 43
			s.Databases[0].OwnerOID = 43
		}},
		{"renamed-and-occupied", "name_collision", "name", func(e *catalogIdentityExpectation, s *catalogNativeSnapshot) {
			s.Roles[0].Name = "external_name"
			s.Roles = append(s.Roles, catalogNativeRole{OID: 43, Name: "customer"})
		}},
		{"nologin-visible", "role_options_changed", "role_options", func(e *catalogIdentityExpectation, s *catalogNativeSnapshot) { s.Roles[0].Flags.Login = false }},
		{"privilege-escalated", "privilege_conflict", "role_options", func(e *catalogIdentityExpectation, s *catalogNativeSnapshot) { s.Roles[0].Flags.Superuser = true }},
		{"marker-changed", "provenance_changed", "provenance", func(e *catalogIdentityExpectation, s *catalogNativeSnapshot) { s.Roles[0].Marker = "external comment" }},
		{"wrong-timeline", "scope_conflict", "", func(e *catalogIdentityExpectation, s *catalogNativeSnapshot) {
			s.Scope.TimelineID = strings.Repeat("3", 32)
		}},
		{"duplicate-role", "observation_invalid", "", func(e *catalogIdentityExpectation, s *catalogNativeSnapshot) { s.Roles = append(s.Roles, s.Roles[0]) }},
		{"unknown-owner", "observation_invalid", "", func(e *catalogIdentityExpectation, s *catalogNativeSnapshot) { s.Databases[0].OwnerOID = 999 }},
		{"noncanonical-digest", "binding_invalid", "", func(e *catalogIdentityExpectation, s *catalogNativeSnapshot) {
			e.MarkerSHA256 = strings.ToUpper(e.MarkerSHA256)
		}},
		{"bad-scope", "binding_invalid", "", func(e *catalogIdentityExpectation, s *catalogNativeSnapshot) { e.Scope.TenantID = "not-a-neon-tenant" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, s := inventoryFixture()
			tc.edit(&e, &s)
			got := compareCatalogNativeIdentity(e, s)
			if got.Status != tc.status || got.Credentials != "not_observed" {
				t.Fatalf("classification=%+v", got)
			}
			if tc.change != "" {
				found := false
				for _, c := range got.Changes {
					found = found || c == tc.change
				}
				if !found {
					t.Fatalf("missing %s: %+v", tc.change, got)
				}
			}
		})
	}
	for _, tc := range []struct {
		name, status string
		edit         func(*catalogNativeSnapshot)
	}{
		{"database-matches", "catalog_matches", func(s *catalogNativeSnapshot) {}},
		{"database-renamed", "renamed", func(s *catalogNativeSnapshot) { s.Databases[0].Name = "new_db" }},
		{"owner-changed", "owner_changed", func(s *catalogNativeSnapshot) { s.Databases[0].OwnerOID = 42 }},
		{"owner-role-renamed", "owner_role_renamed", func(s *catalogNativeSnapshot) { s.Roles[0].Name = "renamed_owner" }},
		{"disabled-database", "database_options_changed", func(s *catalogNativeSnapshot) { s.Databases[0].Flags.AllowConnections = false }},
		{"template-change", "database_options_changed", func(s *catalogNativeSnapshot) { s.Databases[0].Flags.Template = true }},
		{"database-replaced", "replaced", func(s *catalogNativeSnapshot) { s.Databases[0].OID = 52 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, s := inventoryFixture()
			e.Kind = "database"
			e.Name = "customer_db"
			e.OID = 51
			e.OwnerOID = 41
			e.OwnerName = "customer"
			e.MarkerSHA256 = catalogMarkerDigest("managed-db")
			e.DatabaseFlags = s.Databases[0].Flags
			tc.edit(&s)
			if got := compareCatalogNativeIdentity(e, s); got.Status != tc.status {
				t.Fatalf("classification=%+v", got)
			}
		})
	}
}

func TestCatalogInventoryBoundsAndPrivateMarkers(t *testing.T) {
	_, s := inventoryFixture()
	s.Roles[0].Name = " quoted\nexternal "
	if err := validateCatalogNativeSnapshot(s); err != nil {
		t.Fatal("legal external quoted identifier hidden", err)
	}
	raw, err := json.Marshal(s)
	if err != nil || strings.Contains(string(raw), "managed-marker") || strings.Contains(string(raw), "managed-db") {
		t.Fatal("private comments escaped snapshot")
	}
	s.Roles[0].Marker = strings.Repeat("x", 4097)
	if err = validateCatalogNativeSnapshot(s); err == nil {
		t.Fatal("oversize marker accepted")
	}
	_, s = inventoryFixture()
	s.Roles = make([]catalogNativeRole, catalogInventoryLimit+1)
	if !errors.Is(validateCatalogNativeSnapshot(s), errCatalogInventoryBound) {
		t.Fatal("unbounded partial inventory accepted")
	}
}

// This proves catalog SQL against a disposable PostgreSQL catalog. The custom
// GUCs below emulate scope for unit integration; live Neon scope is a separate
// native acceptance and is never claimed by this test.
func TestCatalogInventoryDisposableNativeDDL(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgx.ParseConfig(dsn)
	if dsn == "" || err != nil || cfg.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	role := "inventory_" + randomToken(5)
	renamed := role + "_outside"
	database := role + "_db"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := conn.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec("CREATE ROLE " + pgQuote(role) + " NOLOGIN INHERIT CONNECTION LIMIT 7")
	defer conn.Exec(context.Background(), "DROP ROLE IF EXISTS "+pgQuote(role)+", "+pgQuote(renamed))
	exec("CREATE DATABASE " + pgQuote(database) + " OWNER " + pgQuote(role) + " ALLOW_CONNECTIONS false")
	defer conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgQuote(database))
	scope := catalogNativeScope{strings.Repeat("a", 32), strings.Repeat("b", 32)}
	exec(`SELECT set_config('neon.tenant_id',$1,false),set_config('neon.timeline_id',$2,false)`, scope.TenantID, scope.TimelineID)
	exec("COMMENT ON ROLE " + pgQuote(role) + " IS 'owned inventory marker'")
	observe := func() catalogNativeSnapshot {
		t.Helper()
		v, e := readCatalogNativeSnapshot(ctx, conn, scope)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	before := observe()
	var original catalogNativeRole
	var db catalogNativeDatabase
	for _, r := range before.Roles {
		if r.Name == role {
			original = r
		}
	}
	for _, d := range before.Databases {
		if d.Name == database {
			db = d
		}
	}
	if original.OID == 0 || original.Flags.Login || original.Flags.ConnectionLimit != 7 || db.OID == 0 || db.Flags.AllowConnections || db.OwnerOID != original.OID {
		t.Fatal("NOLOGIN/nonconnectable native objects missing")
	}
	e := catalogIdentityExpectation{Scope: scope, Kind: "role", Name: role, OID: original.OID, MarkerSHA256: catalogMarkerDigest(original.Marker), RoleFlags: original.Flags}
	if got := compareCatalogNativeIdentity(e, before); got.Status != "catalog_matches" {
		t.Fatal(got)
	}
	exec("ALTER ROLE " + pgQuote(role) + " RENAME TO " + pgQuote(renamed))
	after := observe()
	if got := compareCatalogNativeIdentity(e, after); got.Status != "renamed" || got.ObservedOID != original.OID {
		t.Fatal(got)
	}
	exec("CREATE ROLE " + pgQuote(role) + " LOGIN")
	if got := compareCatalogNativeIdentity(e, observe()); got.Status != "name_collision" {
		t.Fatal(got)
	}
	// Even copying the original comment onto a replacement cannot restore OID.
	exec("ALTER DATABASE " + pgQuote(database) + " OWNER TO " + pgQuote(role))
	exec("DROP ROLE " + pgQuote(renamed))
	exec("COMMENT ON ROLE " + pgQuote(role) + " IS 'owned inventory marker'")
	if got := compareCatalogNativeIdentity(e, observe()); got.Status != "replaced" || got.ObservedOID == original.OID {
		t.Fatal(got)
	}
	wrong := scope
	wrong.TimelineID = strings.Repeat("c", 32)
	if _, err = readCatalogNativeSnapshot(ctx, conn, wrong); !errors.Is(err, errCatalogInventoryScope) {
		t.Fatal("cross-scope catalog accepted", err)
	}
	// Oversized comments fail the complete observation, never yield partial data.
	exec(`SELECT 1`)
	exec("COMMENT ON ROLE " + pgQuote(role) + " IS '" + strings.Repeat("x", 4097) + "'")
	if _, err = readCatalogNativeSnapshot(ctx, conn, scope); !errors.Is(err, errCatalogInventoryBound) {
		t.Fatal("oversized catalog accepted", err)
	}
}
