package control

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestDefaultOwnerCredentialEnvelopeScopeAndRotation(t *testing.T) {
	s := &server{backendKeys: &backendKeyring{Active: "v1", decoded: map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32), "v2": bytes.Repeat([]byte{2}, 32)}}}
	scope := defaultOwnerCredentialScope{OperationID: "op_111111111111111111111111", ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", EndpointID: "ep_0123456789abcdef", CredentialUID: "11111111-1111-4111-8111-111111111111", Native: catalogNativeScope{strings.Repeat("a", 32), strings.Repeat("b", 32)}, DatabaseOID: 5}
	const password = "owned-private-current-platform-password"
	version, sealed, err := s.sealDefaultOwnerPassword(scope, password)
	if err != nil || bytes.Contains(sealed, []byte(password)) {
		t.Fatal("credential envelope unavailable or plaintext", err)
	}
	s.backendKeys.Active = "v2"
	if got, e := s.openDefaultOwnerPassword(scope, version, sealed); e != nil || got != password {
		t.Fatal("retained original credential key failed", e)
	}
	for _, change := range []func(*defaultOwnerCredentialScope){
		func(v *defaultOwnerCredentialScope) { v.OperationID = "op_222222222222222222222222" },
		func(v *defaultOwnerCredentialScope) { v.ProjectID = "prj_1123456789abcdef" },
		func(v *defaultOwnerCredentialScope) { v.BranchID = "br_1123456789abcdef" },
		func(v *defaultOwnerCredentialScope) { v.EndpointID = "ep_1123456789abcdef" },
		func(v *defaultOwnerCredentialScope) { v.CredentialUID = "22222222-2222-4222-8222-222222222222" },
		func(v *defaultOwnerCredentialScope) { v.Native.TimelineID = strings.Repeat("c", 32) },
		func(v *defaultOwnerCredentialScope) { v.DatabaseOID = 6 },
	} {
		v := scope
		change(&v)
		if _, err = s.openDefaultOwnerPassword(v, version, sealed); err == nil {
			t.Fatal("cross-scope escrow opened")
		}
	}
	tampered := append([]byte{}, sealed...)
	tampered[len(tampered)-1] ^= 1
	if _, err = s.openDefaultOwnerPassword(scope, version, tampered); err == nil {
		t.Fatal("tampered escrow opened")
	}
	delete(s.backendKeys.decoded, "v1")
	if _, err = s.openDefaultOwnerPassword(scope, version, sealed); err == nil {
		t.Fatal("missing historical key accepted")
	}
}

func TestDefaultOwnerACLRewritesOnlyOwnerAndGrantor(t *testing.T) {
	before := []defaultOwnerACL{{Grantor: 10, Grantee: 10, Privilege: "CREATE", Grantable: true}, {Grantor: 10, Grantee: 20, Privilege: "CREATE"}, {Grantor: 10, Grantee: 0, Privilege: "CONNECT"}, {Grantor: 30, Grantee: 40, Privilege: "TEMPORARY", Grantable: true}}
	after, err := defaultOwnerACLAfter(before, 10, 20)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := canonicalDefaultOwnerACL([]defaultOwnerACL{{Grantor: 20, Grantee: 20, Privilege: "CREATE", Grantable: true}, {Grantor: 20, Grantee: 0, Privilege: "CONNECT"}, {Grantor: 30, Grantee: 40, Privilege: "TEMPORARY", Grantable: true}})
	if err != nil || !equalDefaultOwnerACL(after, expected) {
		t.Fatal("owner ACL mapping changed unrelated grants", err)
	}
	if before[0].Grantor != 10 || before[0].Grantee != 10 {
		t.Fatal("original ACL snapshot changed")
	}
	if _, err = canonicalDefaultOwnerACL([]defaultOwnerACL{{Grantor: 10, Privilege: "SUPERUSER"}}); err == nil {
		t.Fatal("non-database privilege accepted")
	}
	if _, err = canonicalDefaultOwnerACL(make([]defaultOwnerACL, catalogInventoryLimit+1)); err == nil {
		t.Fatal("unbounded ACL accepted")
	}
}

// This uses only disposable control_ci PostgreSQL and a separately created
// platform role. Custom GUCs exercise scope checks, not live Neon acceptance.
func TestDefaultOwnerNativeTransferACLDataCommentAndRollback(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgx.ParseConfig(dsn)
	if dsn == "" || err != nil || cfg.Database != "control_ci" {
		t.Fatal("dedicated disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	const platformPassword = "owned-platform-credential-12345"
	exec("CREATE ROLE cloud_admin LOGIN SUPERUSER PASSWORD '" + platformPassword + "'")
	defer admin.Exec(context.Background(), "DROP ROLE cloud_admin")
	owner := "default_owner_" + randomToken(5)
	other := owner + "_other"
	database := owner + "_db"
	exec("CREATE ROLE " + pgQuote(owner) + " LOGIN INHERIT CREATEROLE CREATEDB REPLICATION BYPASSRLS")
	exec("CREATE ROLE " + pgQuote(other) + " NOLOGIN")
	defer admin.Exec(context.Background(), "DROP ROLE "+pgQuote(owner)+", "+pgQuote(other))
	exec("COMMENT ON ROLE " + pgQuote(owner) + " IS 'neon-control/branch/br_0123456789abcdef'")
	exec("CREATE DATABASE " + pgQuote(database) + " OWNER cloud_admin")
	defer admin.Exec(context.Background(), "DROP DATABASE "+pgQuote(database))
	comment := "arbitrary default database comment; \"retain\" and 'quotes'"
	escaped, err := admin.PgConn().EscapeString(comment)
	if err != nil {
		t.Fatal(err)
	}
	exec("COMMENT ON DATABASE " + pgQuote(database) + " IS '" + escaped + "'")
	exec("GRANT CONNECT ON DATABASE " + pgQuote(database) + " TO " + pgQuote(other))
	exec("GRANT CREATE ON DATABASE " + pgQuote(database) + " TO " + pgQuote(owner))
	platformCfg := cfg.Copy()
	platformCfg.User = "cloud_admin"
	platformCfg.Password = platformPassword
	conn, err := pgx.ConnectConfig(ctx, platformCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	scope := catalogNativeScope{strings.Repeat("a", 32), strings.Repeat("b", 32)}
	if _, err = conn.Exec(ctx, `SELECT set_config('neon.tenant_id',$1,false),set_config('neon.timeline_id',$2,false)`, scope.TenantID, scope.TimelineID); err != nil {
		t.Fatal(err)
	}
	p := defaultOwnerSQLIntent{Scope: scope, Database: database, PlatformRole: "cloud_admin", PreviousOwner: "cloud_admin", Owner: owner, BranchID: "br_0123456789abcdef", CommentSHA256: catalogMarkerDigest(comment), Flags: catalogDatabaseFlags{AllowConnections: true, ConnectionLimit: -1}, TargetRoleFlags: catalogRoleFlags{Login: true, Inherit: true, CreateRole: true, CreateDB: true, Replication: true, BypassRLS: true, ConnectionLimit: -1}, TargetMarkerSHA256: catalogMarkerDigest("neon-control/branch/br_0123456789abcdef")}
	if err = conn.QueryRow(ctx, `SELECT (SELECT oid FROM pg_roles WHERE rolname='cloud_admin'),(SELECT oid FROM pg_roles WHERE rolname=$1),(SELECT oid FROM pg_database WHERE datname=$2)`, owner, database).Scan(&p.PlatformRoleOID, &p.OwnerOID, &p.DatabaseOID); err != nil {
		t.Fatal(err)
	}
	p.PreviousOwnerOID = p.PlatformRoleOID
	acl := func() []defaultOwnerACL {
		t.Helper()
		tx, e := conn.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		v, e := readDefaultOwnerACL(ctx, tx, p.DatabaseOID)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	p.PreviousACL = acl()
	dataCfg := platformCfg.Copy()
	dataCfg.Database = database
	data, err := pgx.ConnectConfig(ctx, dataCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close(context.Background())
	if _, err = data.Exec(ctx, `CREATE SCHEMA default_owner_probe AUTHORIZATION cloud_admin; CREATE TABLE default_owner_probe.retained(id integer PRIMARY KEY,marker text); INSERT INTO default_owner_probe.retained VALUES(1,'original-default-owner-data')`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	readData := func() []string {
		t.Helper()
		var marker, schemaOwner, tableOwner, tableOID string
		if e := data.QueryRow(ctx, `SELECT (SELECT marker FROM default_owner_probe.retained WHERE id=1),n.nspowner::text,c.relowner::text,c.oid::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='default_owner_probe' AND c.relname='retained'`).Scan(&marker, &schemaOwner, &tableOwner, &tableOID); e != nil {
			t.Fatal(e)
		}
		return []string{marker, schemaOwner, tableOwner, tableOID}
	}
	originalData := readData()
	// Owner changes use the fixed maintenance database. A disabled default
	// database remains disabled; existing data/session owners are unchanged.
	exec("ALTER DATABASE " + pgQuote(database) + " ALLOW_CONNECTIONS false")
	p.Flags.AllowConnections = false
	if err = applyDefaultDatabaseOwnerSQL(ctx, conn, p); err != nil {
		t.Fatal("native Owner transfer failed", err)
	}
	if err = applyDefaultDatabaseOwnerSQL(ctx, conn, p); err != nil {
		t.Fatal("unknown native commit replay failed", err)
	}
	if _, err = conn.Exec(ctx, "SET ROLE "+pgQuote(other)); err != nil {
		t.Fatal(err)
	}
	if achieved, e := observeDefaultOwnerSQL(ctx, conn, p, true); e != nil || !achieved {
		t.Fatal("unprivileged original postcondition observation failed", e)
	}
	if e := applyDefaultDatabaseOwnerSQL(ctx, conn, p); e == nil {
		t.Fatal("unprivileged observer performed privileged Owner SQL")
	}
	if _, err = conn.Exec(ctx, "RESET ROLE"); err != nil {
		t.Fatal(err)
	}
	for i, v := range readData() {
		if v != originalData[i] {
			t.Fatal("schema/table owner or data changed")
		}
	}
	targetACL, err := defaultOwnerACLAfter(p.PreviousACL, p.PreviousOwnerOID, p.OwnerOID)
	if err != nil || !equalDefaultOwnerACL(acl(), targetACL) {
		t.Fatal("unrelated ACL grant changed", err)
	}
	restore := p
	restore.PreviousOwner = p.Owner
	restore.PreviousOwnerOID = p.OwnerOID
	restore.Owner = "cloud_admin"
	restore.OwnerOID = p.PlatformRoleOID
	restore.Platform = true
	restore.PreviousACL = acl()
	rejected := func(value defaultOwnerSQLIntent) {
		t.Helper()
		if e := applyDefaultDatabaseOwnerSQL(ctx, conn, value); e == nil {
			t.Fatal("conflicted default Owner accepted")
		}
		var current uint32
		if e := conn.QueryRow(ctx, `SELECT datdba FROM pg_database WHERE oid=$1`, p.DatabaseOID).Scan(&current); e != nil || current != p.OwnerOID {
			t.Fatal("rejected transfer changed Owner", e)
		}
	}
	wrong := restore
	wrong.Scope.TimelineID = strings.Repeat("c", 32)
	rejected(wrong)
	wrong = restore
	wrong.DatabaseOID++
	rejected(wrong)
	wrong = restore
	wrong.OwnerOID++
	rejected(wrong)
	exec("COMMENT ON DATABASE " + pgQuote(database) + " IS 'external comment'")
	rejected(restore)
	exec("COMMENT ON DATABASE " + pgQuote(database) + " IS '" + escaped + "'")
	exec("REVOKE CONNECT ON DATABASE " + pgQuote(database) + " FROM " + pgQuote(other))
	rejected(restore)
	// A fresh, explicitly observed ACL is a new intent; it is not overwritten by
	// replaying an older operation's expectations.
	restore.PreviousACL = acl()
	if err = applyDefaultDatabaseOwnerSQL(ctx, conn, restore); err != nil {
		t.Fatal("platform Owner restore failed", err)
	}
	for i, v := range readData() {
		if v != originalData[i] {
			t.Fatal("platform restore changed retained data")
		}
	}
}
