package control

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

func TestFunctionManagerRetainedBootstrapMACStillRequired(t *testing.T) {
	ref, b := functionBootstrapFixture(t)
	key := bytes.Repeat([]byte{7}, 32)
	now := time.Now()
	root, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "archival-bootstrap-disposable"}, NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &root.PublicKey, root)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(root)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := functions.NewGuestIssuer(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), now)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := issuer.Issue(b.Scope, now.Add(-3*time.Hour), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b.ManagerCA, b.ManagerCertificate, b.ManagerPrivateKey = leaf.CA, leaf.Certificate, leaf.PrivateKey
	if _, _, err = functionBootstrapContent(key, ref, b); err == nil {
		t.Fatal("expired leaf was admitted as a new bootstrap")
	}
	mac, raw, err := functionBootstrapCanonicalContent(key, ref, b)
	if err != nil {
		t.Fatal(err)
	}
	ref.UID, ref.MAC = "11111111-2222-4333-8444-555555555555", mac
	item := map[string]any{"type": "Opaque", "immutable": true, "metadata": map[string]any{"name": ref.Name, "namespace": ref.Namespace, "uid": ref.UID, "labels": functionBootstrapLabels(ref), "annotations": map[string]string{"neon-control/bootstrap-key-version": ref.MACKeyVersion, "neon-control/bootstrap-mac": mac}}, "data": map[string]any{"config.json": base64.StdEncoding.EncodeToString(raw)}}
	// Match Kubernetes JSON decoding, including nested label/annotation maps.
	wire, err := json.Marshal(item)
	if err != nil || json.Unmarshal(wire, &item) != nil {
		t.Fatal("invalid Secret wire fixture")
	}
	if _, err = functions.DecodeRetainedBootstrap(raw); err != nil {
		t.Fatal("expired signed receipt validation failed", err)
	}
	retained, err := verifyFunctionBootstrap(item, key, ref)
	if err != nil || retained.Config.ManagerCertificate != b.ManagerCertificate || retained.Reference.UID != ref.UID {
		t.Fatal("expired original receipt could not be read with exact trust and MAC", err)
	}
	if _, err = functions.DecodeBootstrap(raw); err == nil {
		t.Fatal("archival read admitted an expired new Guest boot")
	}
	b.Environment["GREETING"] = "tampered-disposable-value"
	changed, _ := json.Marshal(b)
	item["data"].(map[string]any)["config.json"] = base64.StdEncoding.EncodeToString(changed)
	if _, err = verifyFunctionBootstrap(item, key, ref); err == nil {
		t.Fatal("archival path accepted content without original MAC")
	}
	item["data"].(map[string]any)["config.json"] = base64.StdEncoding.EncodeToString(raw)
	ref.UID = "22222222-2222-4333-8444-555555555555"
	if _, err = verifyFunctionBootstrap(item, key, ref); err == nil {
		t.Fatal("archival path adopted a replacement Secret UID")
	}
}

func TestFunctionManagerIdentityHistoryAndIntent(t *testing.T) {
	f := newStorageEventFixture(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.db.Exec(f.ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	const instance = "fni_3123456789abcdef"
	const function = "fnc_1123456789abcdef"
	const deployment = "fdp_1123456789abcdef"
	const vm = "11111111-1111-4111-8111-111111111111"
	const svc = "22222222-2222-4222-8222-222222222222"
	const bootSecret = "33333333-3333-4333-8333-333333333333"
	boot := strings.Repeat("a", 32)
	exec(`INSERT INTO function_instances(id,function_id,deployment_id,project_id,branch_id,generation,vm_name,service_name,bootstrap_secret_ref,state,vm_uid,service_uid,boot_id)
 VALUES($1,$2,$3,$4,$5,1,'fn-3123456789abcdef','fn-3123456789abcdef','fn-3123456789abcdef-bootstrap','ready',$6,$7,$8)`, instance, function, deployment, f.p.ProjectID, f.p.BranchID, vm, svc, boot)
	exec(`INSERT INTO function_instance_bootstraps(instance_id,function_id,deployment_id,project_id,branch_id,namespace,secret_name,secret_uid,content_mac,mac_key_version)
 VALUES($1,$2,$3,$4,$5,'neon','fn-3123456789abcdef-bootstrap',$6,repeat('b',64),'test-v1')`, instance, function, deployment, f.p.ProjectID, f.p.BranchID, bootSecret)
	exec(`UPDATE function_definitions SET state='active' WHERE id=$1`, function)
	scope := functions.Scope{InstanceID: instance, DeploymentID: deployment, ProjectID: f.p.ProjectID, BranchID: f.p.BranchID, Slug: "onupload", Generation: 1}
	runtime := publishedFunctionRuntime{target: functionInstanceTarget{FunctionID: function, ProjectID: f.p.ProjectID, BranchID: f.p.BranchID, DeploymentID: deployment, Generation: 1, Slug: "onupload"}, bootID: boot}
	vmID, svcID := vm, svc
	runtime.instance = functionInstanceRecord{Scope: scope, VMUID: &vmID, ServiceUID: &svcID}
	runtime.bootstrap.UID = bootSecret
	if err := f.db.QueryRow(f.ctx, `SELECT version FROM function_definitions WHERE id=$1`, function).Scan(&runtime.version); err != nil {
		t.Fatal(err)
	}
	old := functions.ManagerIdentity{Scope: scope, BootID: boot, Fingerprint: strings.Repeat("c", 64), NotBefore: time.Now().UTC().Truncate(time.Second).Add(-time.Minute), NotAfter: time.Now().UTC().Truncate(time.Second).Add(time.Hour)}
	if err := f.s.rememberFunctionManagerIdentity(f.ctx, runtime, old); err != nil {
		t.Fatal("original identity observation rejected", err)
	}
	next := old
	next.Fingerprint = strings.Repeat("d", 64)
	next.NotAfter = next.NotAfter.Add(time.Hour)
	next.Renewals = 1
	for n := 0; n < 2; n++ {
		if err := f.s.rememberFunctionManagerIdentity(f.ctx, runtime, next); err != nil {
			t.Fatal("renewal observation or same identity replay rejected", err)
		}
	}
	var count int
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM function_manager_identities WHERE instance_id=$1`, instance).Scan(&count); err != nil || count != 2 {
		t.Fatal("immutable observations duplicated or lost", err)
	}
	for _, kind := range []string{"boot", "scope", "vm", "service", "bootstrap", "version", "counter-reuse"} {
		t.Run(kind, func(t *testing.T) {
			r, v := runtime, next
			v.Fingerprint = strings.Repeat("e", 64)
			v.Renewals = 2
			switch kind {
			case "boot":
				v.BootID = strings.Repeat("f", 32)
			case "scope":
				v.Scope.Generation++
			case "vm":
				id := "44444444-4444-4444-8444-444444444444"
				r.instance.VMUID = &id
			case "service":
				id := "44444444-4444-4444-8444-444444444444"
				r.instance.ServiceUID = &id
			case "bootstrap":
				r.bootstrap.UID = "44444444-4444-4444-8444-444444444444"
			case "version":
				r.version++
			case "counter-reuse":
				v.Renewals = 1
			}
			if err := f.s.rememberFunctionManagerIdentity(f.ctx, r, v); err == nil {
				t.Fatal("stale or foreign identity was recorded")
			}
		})
	}
	if _, err := f.db.Exec(f.ctx, `UPDATE function_manager_identities SET not_after=not_after+interval '1 minute' WHERE instance_id=$1 AND renewals=0`, instance); err == nil {
		t.Fatal("old certificate history was rewritten")
	}
	exec(`UPDATE function_definitions SET state='suspended' WHERE id=$1`, function)
	if err := f.s.rememberFunctionManagerIdentity(f.ctx, runtime, next); err == nil {
		t.Fatal("suspended intent accepted certificate observation")
	}
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM function_manager_identities WHERE instance_id=$1`, instance).Scan(&count); err != nil || count != 2 {
		t.Fatal("rejected observations changed history", err)
	}
}

func TestFunctionManagerRenewalWindowIsOperatorBounded(t *testing.T) {
	t.Setenv("NEON_FUNCTION_MANAGER_RENEW_BEFORE_SECONDS", "")
	if value, err := functionManagerRenewBefore(); err != nil || value != 45*time.Minute {
		t.Fatal("default renewal window changed", err)
	}
	for _, value := range []string{"599", "7141", "-1", "unknown"} {
		t.Setenv("NEON_FUNCTION_MANAGER_RENEW_BEFORE_SECONDS", value)
		if _, err := functionManagerRenewBefore(); err == nil {
			t.Fatal("unbounded renewal window accepted")
		}
	}
	t.Setenv("NEON_FUNCTION_MANAGER_RENEW_BEFORE_SECONDS", "7140")
	if value, err := functionManagerRenewBefore(); err != nil || value != 119*time.Minute {
		t.Fatal("bounded functional window rejected", err)
	}
}
