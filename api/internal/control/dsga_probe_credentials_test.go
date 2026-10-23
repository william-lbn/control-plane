package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestDSGAProbeCredentialSnapshotAndLegacyReadOnly(t *testing.T) {
	secret := credentialSourceSecret()
	legacy, err := snapshotDSGACredentials(secret, secret.Namespace, secret.Name, "prj_test", "ep_0123456789abcdef")
	if err != nil {
		t.Fatal("historical source no longer readable")
	}
	if password, err := legacy.probePassword(); !errors.Is(err, errDSGACredentialSource) || password != "" {
		t.Fatal("historical verifier fabricated a client password")
	}
	seed := bytes.Repeat([]byte{17}, 32)
	secret.Data["probeCredentialSeed"] = seed
	secret.Data["probeVerifier"] = []byte(dsgaVerifier(base64.RawURLEncoding.EncodeToString(seed), bytes.Repeat([]byte{3}, 16)))
	snapshot, err := snapshotDSGACredentials(secret, secret.Namespace, secret.Name, "prj_test", "ep_0123456789abcdef")
	if err != nil || snapshot.reference() == legacy.reference() {
		t.Fatal("new private field not part of frozen content identity")
	}
	password, err := snapshot.probePassword()
	if err != nil || !dsgaPasswordMatches(password, snapshot.credentials.ProbeVerifier) {
		t.Fatal("snapshot cannot authenticate its own probe verifier")
	}
	secret.Data["probeCredentialSeed"][0] ^= 1
	if again, err := snapshot.probePassword(); err != nil || again != password {
		t.Fatal("snapshot retained mutable seed bytes")
	}
	if _, err := snapshotDSGACredentials(secret, secret.Namespace, secret.Name, "prj_test", "ep_0123456789abcdef"); err == nil {
		t.Fatal("probe seed/verifier mismatch accepted")
	}
	secret.Data["probeCredentialSeed"] = []byte{1}
	if _, err := snapshotDSGACredentials(secret, secret.Namespace, secret.Name, "prj_test", "ep_0123456789abcdef"); err == nil {
		t.Fatal("short probe seed accepted")
	}
	if data, err := makeDSGACredentialData("", legacy, true, bytes.NewReader(bytes.Repeat([]byte{7}, 32))); err == nil || data != nil {
		t.Fatal("legacy source silently rotated replica probe identity")
	}
}

func TestDSGAProbeCredentialLegacyProducerRefusesRewrite(t *testing.T) {
	p, _, plan := dsgaPlanFixture(t)
	f := newCredentialProducerAPI(t, "success")
	input := dsgaCredentialInput{Password: "writer-legacy-candidate-password"}
	frozen, err := f.kube.prepareDSGACredentials(context.Background(), p, plan, "op_original", input)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	secret := f.objects[kubeName(p.EndpointID)+"-credentials"]
	delete(secret.Data, "probeCredentialSeed") // 模拟此前三键候选；生产不允许此更新。
	legacy, err := snapshotDSGACredentials(secret, secret.Namespace, secret.Name, p.ProjectID, p.EndpointID)
	before, _ := secret.Marshal()
	f.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	frozen.Credentials = legacy.reference()
	for _, original := range []dsgaCreationPlan{plan, frozen} {
		if _, err := f.kube.prepareDSGACredentials(context.Background(), p, original, "op_original", input); err == nil {
			t.Fatal("producer accepted source with irrecoverable probe credential")
		}
	}
	f.mu.Lock()
	after, _ := f.objects[secret.Name].Marshal()
	posts := f.posts
	f.mu.Unlock()
	if posts != 1 || !bytes.Equal(before, after) {
		t.Fatal("legacy retry rewrote immutable original source")
	}
}

// 实际typed生产者→完整来源摘要→安全Renderer→真实PG17 SCRAM。这里验证探针
// 密码可恢复和权限；TLS API是协议夹具，不冒称实际K8s/Proxy或物理副本验收。
// 不重复既有四类应用口令测试，新增的客户端证明只针对独立probe身份。
func TestDSGAProbeCredentialNativeClientsAndReplicaInheritance(t *testing.T) {
	url, psql := os.Getenv("NEON_V2_TEST_DATABASE_URL"), os.Getenv("DSGA_TEST_PSQL")
	if url == "" || psql == "" {
		t.Skip("native owned PG and explicit libpq psql required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal("owned PG connection failed")
	}
	defer func() { _ = admin.Close(context.Background()) }()
	f := newCredentialProducerAPI(t, "success")
	p, _, plan := dsgaPlanFixture(t)
	plan, err = f.kube.prepareDSGACredentials(ctx, p, plan, "op_probe_writer", dsgaCredentialInput{Password: "independent-application-password"})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	writerSecret := f.objects[kubeName(p.EndpointID)+"-credentials"].DeepCopy()
	f.mu.Unlock()
	writer, err := snapshotDSGACredentials(writerSecret, plan.Namespace, writerSecret.Name, p.ProjectID, p.EndpointID)
	if err != nil {
		t.Fatal(err)
	}
	role := newID("dsga_probe_")
	if _, err := admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 4 PASSWORD "+pgLiteral(writer.credentials.ProbeVerifier)); err != nil {
		t.Fatal("probe role creation failed")
	}
	defer func() {
		if _, err := admin.Exec(ctx, "DROP ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
			t.Error("owned probe role cleanup failed")
		}
	}()
	for _, replica := range []bool{false, true} {
		name := "writer"
		if replica {
			name = "replica"
		}
		t.Run(name, func(t *testing.T) {
			snapshot, secret := writer, writerSecret
			target, targetPlan := p, plan
			if replica {
				target.EndpointID, target.EndpointType = "ep_probe_replica", "read_only"
				targetPlan, err = newDSGACreationPlan(target, dsgaCreationPolicy{Namespace: plan.Namespace, Image: plan.Image, MemorySlotBytes: plan.MemorySlotBytes, Budget: plan.Budget}, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				targetPlan, err = f.kube.prepareDSGACredentials(ctx, target, targetPlan, "op_probe_replica", dsgaCredentialInput{SourceEndpoint: p.EndpointID, Source: plan.Credentials})
				if err != nil {
					t.Fatal(err)
				}
				f.mu.Lock()
				secret = f.objects[kubeName(target.EndpointID)+"-credentials"].DeepCopy()
				f.mu.Unlock()
				snapshot, err = snapshotDSGACredentials(secret, plan.Namespace, secret.Name, p.ProjectID, target.EndpointID)
				if err != nil || bytes.Equal(secret.Data["controlSigningSeed"], writerSecret.Data["controlSigningSeed"]) {
					t.Fatal("replica reused management identity")
				}
			}
			password, err := snapshot.probePassword()
			original, _ := writer.probePassword()
			if err != nil || password != original || dsgaPasswordMatches(password, snapshot.credentials.ApplicationVerifier) {
				t.Fatal("probe client identity rotated or reused application identity")
			}
			config, err := renderDSGAComputeBootstrap(target, targetPlan, "vm_probe_original", snapshot.credentials)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{password, base64.StdEncoding.EncodeToString(secret.Data["probeCredentialSeed"]), base64.StdEncoding.EncodeToString(secret.Data["controlSigningSeed"]), "probeCredentialSeed"} {
				if bytes.Contains(config, []byte(private)) {
					t.Fatal("private client credential entered Guest config")
				}
			}
			client, err := pgx.ParseConfig(url)
			if err != nil {
				t.Fatal("invalid owned PG config")
			}
			client.User, client.Password = role, password
			conn, err := pgx.ConnectConfig(ctx, client)
			if err != nil {
				t.Fatal("probe pgx SCRAM authentication failed")
			}
			var actual string
			var privileged bool
			err = conn.QueryRow(ctx, `SELECT current_user,rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&actual, &privileged)
			_ = conn.Close(ctx)
			if err != nil || actual != role || privileged {
				t.Fatal("probe authenticated with wrong or privileged SQL identity")
			}
			cmd := exec.CommandContext(ctx, psql, "-X", "-w", "-At", "-c", "SELECT current_user")
			cmd.Env = append(os.Environ(), "PGHOST="+client.Host, "PGPORT="+strconv.Itoa(int(client.Port)), "PGDATABASE="+client.Database, "PGUSER="+role, "PGPASSWORD="+password, "PGSSLMODE=disable")
			out, err := cmd.Output()
			if err != nil || strings.TrimSpace(string(out)) != role {
				t.Fatal("probe libpq SCRAM authentication failed")
			}
		})
	}
}
