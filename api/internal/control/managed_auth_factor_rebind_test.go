package control

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestManagedAuthFactorRebindAtomicNativeAndPublication(t *testing.T) {
	ctx, s, exec := catalogBindingFixture(t)
	s.idempotencyKey = []byte(strings.Repeat("synthetic-factor-root-key", 3))
	parent := "br_0123456789abcdef"
	child := "br_fedcba9876543210"
	exec(`INSERT INTO branches(id,project_id,parent_branch_id,name,timeline_id,state,created_at) VALUES($1,'prj_0123456789abcdef',$2,'factor-child',$3,'ready',now())`, child, parent, strings.Repeat("c", 32))
	p := managedAuthPayload{ProjectID: "prj_0123456789abcdef", BranchID: child, EndpointID: "ep_0123456789abcdef", Generation: 1, FactorSourceBranchID: parent, FactorOperationID: "op_synthetic_factor_child", Spec: ManagedAuthSpec{Database: "control_ci", AllowedOrigins: []string{}, TwoFactorEnabled: true, InheritTwoFactor: true}}
	source := p
	source.BranchID = parent
	source.Spec.InheritTwoFactor = false
	source.FactorSourceBranchID = ""
	source.FactorOperationID = ""
	raw, _ := json.Marshal(source)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES('op_synthetic_factor_parent',$1,'managed_auth',$2,'enable_managed_auth','succeeded','binding_admin','synthetic',$3)`, p.ProjectID, parent, raw)
	raw, _ = json.Marshal(p)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,'managed_auth',$3,'enable_managed_auth','running','binding_admin','synthetic',$4)`, p.FactorOperationID, p.ProjectID, child, raw)
	admin, err := pgx.Connect(ctx, os.Getenv("NEON_V2_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	// Separate disposable PostgreSQL databases model copied native schemas. The
	// physical Neon timeline and real issuer login are checked by live acceptance.
	suffix := strings.TrimPrefix(newID("factor_"), "factor_")
	parentDB, childDB := "factor_parent_"+suffix, "factor_child_"+suffix
	for _, db := range []string{parentDB, childDB} {
		if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgQuote(db)); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, db := range []string{childDB, parentDB} {
			admin.Exec(context.Background(), "DROP DATABASE "+pgQuote(db)+" WITH (FORCE)")
		}
		for _, branch := range []string{child, parent} {
			admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+pgQuote(managedAuthLogin(branch)))
		}
	}()
	connect := func(database string) *pgx.Conn {
		u, _ := url.Parse(os.Getenv("NEON_V2_TEST_DATABASE_URL"))
		u.Path = "/" + database
		c, e := pgx.Connect(ctx, u.String())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { c.Close(context.Background()) })
		return c
	}
	verifier, err := scramVerifier("synthetic-runtime-password-only")
	if err != nil {
		t.Fatal(err)
	}
	parentConn := connect(parentDB)
	nativeExec := func(c *pgx.Conn, q string, args ...any) {
		t.Helper()
		if _, e := c.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	setup := func(c *pgx.Conn) {
		tx, e := c.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if e = applyManagedAuthSQLTx(ctx, tx, source, verifier); e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
	}
	setup(parentConn)
	parentKey, _ := s.managedAuthTOTPSecret(source)
	childKey, _ := s.managedAuthTOTPSecret(p)
	factor, _ := sealManagedAuthFactor(parentKey, []byte("JBSWY3DPEHPK3PXP"), true)
	backup, _ := sealManagedAuthFactor(parentKey, []byte(`["synthetic-recovery-code"]`), true)
	nativeExec(parentConn, `INSERT INTO neon_auth."user"(id,name,email,"emailVerified","twoFactorEnabled") VALUES('confirmed','Confirmed','confirmed@example.test',true,true),('pending','Pending','pending@example.test',true,false),('plain','Plain','plain@example.test',true,false)`)
	nativeExec(parentConn, `INSERT INTO neon_auth."twoFactor"(id,secret,"backupCodes","userId",verified,"failedVerificationCount","lockedUntil") VALUES('confirmed-factor',$1,$2,'confirmed',true,2,now()+interval '1 minute'),('pending-factor',$1,$2,'pending',false,0,NULL)`, factor, backup)
	nativeExec(parentConn, `INSERT INTO neon_auth.account(id,"accountId","providerId","userId",password,"updatedAt") VALUES('credential','confirmed','credential','confirmed','synthetic-password-hash',now()); INSERT INTO neon_auth.session(id,"expiresAt",token,"updatedAt","userId") VALUES('parent-session',now()+interval '1 hour','synthetic-session',now(),'confirmed'); INSERT INTO neon_auth.verification(id,identifier,value,"expiresAt") VALUES('parent-reset','synthetic-reset','synthetic-value',now()+interval '1 hour')`, pgx.QueryExecModeSimpleProtocol)
	// PostgreSQL template copy requires all source connections closed.
	parentConn.Close(ctx)
	nativeExec(admin, "DROP DATABASE "+pgQuote(childDB))
	nativeExec(admin, "CREATE DATABASE "+pgQuote(childDB)+" TEMPLATE "+pgQuote(parentDB))
	parentConn = connect(parentDB)
	conn := connect(childDB)
	apply := func(value managedAuthPayload) (*managedAuthFactorProof, error) {
		tx, e := conn.Begin(ctx)
		if e != nil {
			return nil, e
		}
		defer tx.Rollback(ctx)
		proof, e := s.rebindManagedAuthFactorsTx(ctx, tx, value)
		if e != nil {
			return nil, e
		}
		if e = applyManagedAuthSQLWithFactorsTx(ctx, tx, value, verifier, true); e != nil {
			return nil, e
		}
		return proof, tx.Commit(ctx)
	}
	// Wrong source history or key, inconsistent rows, and late SQL failures must
	// leave the original ciphertext, pending rows, and sessions unchanged.
	assertUntouched := func() {
		t.Helper()
		var cipher string
		var n int
		if e := conn.QueryRow(ctx, `SELECT secret FROM neon_auth."twoFactor" WHERE id='confirmed-factor'`).Scan(&cipher); e != nil || cipher != factor {
			t.Fatal("failed migration altered factor", e)
		}
		if e := conn.QueryRow(ctx, `SELECT count(*) FROM neon_auth.session`).Scan(&n); e != nil || n != 1 {
			t.Fatal("failed migration cleared session", e)
		}
	}
	wrong := p
	wrong.FactorSourceBranchID = "br_aaaaaaaaaaaaaaaa"
	if _, err = apply(wrong); err == nil {
		t.Fatal("unrelated parent accepted")
	}
	assertUntouched()
	exec(`UPDATE operations SET state='failed' WHERE id='op_synthetic_factor_parent'`)
	if _, err = apply(p); err == nil {
		t.Fatal("unverified source history accepted")
	}
	assertUntouched()
	exec(`UPDATE operations SET state='succeeded' WHERE id='op_synthetic_factor_parent'`)
	nativeExec(conn, `UPDATE neon_auth."twoFactor" SET secret='00' WHERE id='confirmed-factor'`)
	if _, err = apply(p); err == nil {
		t.Fatal("corrupt ciphertext accepted")
	}
	nativeExec(conn, `UPDATE neon_auth."twoFactor" SET secret=$1 WHERE id='confirmed-factor'`, factor)
	assertUntouched()
	// A conflict discovered after rewrapping must roll the entire native tx back.
	nativeExec(admin, "CREATE ROLE "+pgQuote(managedAuthLogin(child))+" NOLOGIN")
	if _, err = apply(p); err == nil {
		t.Fatal("runtime role ownership conflict ignored")
	}
	assertUntouched()
	nativeExec(admin, "DROP ROLE "+pgQuote(managedAuthLogin(child)))
	proof, err := apply(p)
	if err != nil {
		t.Fatal(err)
	}
	if proof.Migrated != 1 || proof.PendingRemoved != 1 || !proof.RecoveryCodesReset {
		t.Fatal("migration accounting incorrect")
	}
	var actual, actualBackup string
	var enabled bool
	var failed int
	if err = conn.QueryRow(ctx, `SELECT f.secret,f."backupCodes",u."twoFactorEnabled",f."failedVerificationCount" FROM neon_auth."twoFactor" f JOIN neon_auth."user" u ON u.id=f."userId" WHERE f.id='confirmed-factor'`).Scan(&actual, &actualBackup, &enabled, &failed); err != nil {
		t.Fatal(err)
	}
	plain, err := openManagedAuthFactor(childKey, actual)
	if err != nil || string(plain) != "JBSWY3DPEHPK3PXP" || !enabled || failed != 2 {
		t.Fatal("verified factor or lockout state changed")
	}
	clear(plain)
	if _, err = openManagedAuthFactor(parentKey, actual); err == nil {
		t.Fatal("child factor accepted parent key")
	}
	plain, err = openManagedAuthFactor(childKey, actualBackup)
	if err != nil || string(plain) != "[]" {
		t.Fatal("inherited recovery codes remained valid")
	}
	clear(plain)
	var counts []int
	if err = conn.QueryRow(ctx, `SELECT ARRAY[(SELECT count(*)::int FROM neon_auth."twoFactor"),(SELECT count(*)::int FROM neon_auth.session),(SELECT count(*)::int FROM neon_auth.verification),(SELECT count(*)::int FROM neon_auth.account WHERE password='synthetic-password-hash'),(SELECT count(*)::int FROM neon_auth."user")]`).Scan(&counts); err != nil || len(counts) != 5 || counts[0] != 1 || counts[1] != 0 || counts[2] != 0 || counts[3] != 1 || counts[4] != 3 {
		t.Fatal("native account/session boundaries failed", counts, err)
	}
	var parentFactor string
	var parentPending int
	if err = parentConn.QueryRow(ctx, `SELECT (SELECT secret FROM neon_auth."twoFactor" WHERE id='confirmed-factor'),(SELECT count(*)::int FROM neon_auth."twoFactor")`).Scan(&parentFactor, &parentPending); err != nil || parentFactor != factor || parentPending != 2 {
		t.Fatal("parent database was modified")
	}
	var allowed bool
	if err = conn.QueryRow(ctx, `SELECT has_table_privilege($1,'neon_auth.control_factor_rebindings','SELECT,INSERT,UPDATE,DELETE')`, managedAuthLogin(child)).Scan(&allowed); err != nil || allowed {
		t.Fatal("runtime gained control receipt privileges", err)
	}
	if err = s.publishManagedAuthFactorProof(ctx, p, proof); err != nil {
		t.Fatal(err)
	}
	if err = s.publishManagedAuthFactorProof(ctx, p, proof); err != nil {
		t.Fatal("publication replay", err)
	}
	// Simulate a successful user factor replacement after native commit but
	// before the worker observes publication. Retry must retain the replacement.
	fresh, _ := sealManagedAuthFactor(childKey, []byte("KRUGS4ZANFZSAYJA"), false)
	nativeExec(conn, `UPDATE neon_auth."twoFactor" SET secret=$1 WHERE id='confirmed-factor'`, fresh)
	replayed, err := apply(p)
	if err != nil || *replayed != *proof {
		t.Fatal("original native receipt not recovered", err)
	}
	if err = conn.QueryRow(ctx, `SELECT secret FROM neon_auth."twoFactor" WHERE id='confirmed-factor'`).Scan(&actual); err != nil || actual != fresh {
		t.Fatal("retry overwrote new user enrollment")
	}
	p.Generation = 3
	p.FactorOperationID = "op_synthetic_factor_reenable"
	if _, err = apply(p); err != nil {
		t.Fatal("same child re-enable rotated factor", err)
	}
	if _, err = conn.Exec(ctx, `UPDATE neon_auth.control_factor_rebindings SET mac=repeat('0',64)`); err == nil {
		t.Fatal("native receipt mutable")
	}
	if _, err = s.db.Exec(ctx, `DELETE FROM managed_auth_factor_results WHERE operation_id=$1`, proof.OperationID); err == nil {
		t.Fatal("metadata receipt mutable")
	}
	encoded, _ := json.Marshal(map[string]string{"secret": childKey, "ciphertext": fresh, "plaintext": "KRUGS4ZANFZSAYJA"})
	t.Log("SYNTHETIC_REWRAPPED_VECTOR " + string(encoded))
}

func TestManagedAuthFactorInheritancePolicyAndReceiptScope(t *testing.T) {
	p := managedAuthPayload{ProjectID: "prj_0123456789abcdef", BranchID: "br_0123456789abcdef", Generation: 1, FactorSourceBranchID: "br_fedcba9876543210", Spec: ManagedAuthSpec{Database: "neondb", AllowedOrigins: []string{}, InheritTwoFactor: true}}
	if validateManagedAuth(p) == nil {
		t.Fatal("inheritance disabled TOTP")
	}
	p.Spec.TwoFactorEnabled = true
	if err := validateManagedAuth(p); err != nil {
		t.Fatal(err)
	}
	s := &server{idempotencyKey: []byte(strings.Repeat("synthetic-root", 4))}
	proof := managedAuthFactorProof{Version: 1, ProjectID: p.ProjectID, BranchID: p.BranchID, Database: p.Spec.Database, SourceBranchID: p.FactorSourceBranchID, SourceGeneration: 1, TargetGeneration: 1, OperationID: "op_synthetic", SourceDigest: strings.Repeat("a", 64), TargetDigest: strings.Repeat("b", 64), RecoveryCodesReset: true, InheritedLoginsDisabled: 1}
	mac := s.managedAuthFactorMAC(proof)
	if !s.validManagedAuthFactorProof(proof, mac, p) {
		t.Fatal("original proof rejected")
	}
	proof.Migrated++
	if s.validManagedAuthFactorProof(proof, mac, p) {
		t.Fatal("changed count authenticated")
	}
	proof.Migrated--
	p.BranchID = "br_aaaaaaaaaaaaaaaa"
	if s.validManagedAuthFactorProof(proof, mac, p) {
		t.Fatal("foreign target accepted")
	}
	raw, _ := json.Marshal(ManagedAuthSpec{Database: "neondb", AllowedOrigins: []string{}})
	if strings.Contains(string(raw), "inherit_two_factor") {
		t.Fatal("legacy request hash representation changed")
	}
}
