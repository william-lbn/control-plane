package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// 真实TLS/typed客户端，服务端仅模拟API协议与持久对象，不声称K8s认证。
// 不输出Secret body，所有对象读写在同一锁下，race验证覆盖恢复读取。
type credentialProducerAPI struct {
	mu        sync.Mutex
	objects   map[string]*core.Secret
	posts     int
	mode      string
	kube      *kubeClient
	afterPost func()
}

func newCredentialProducerAPI(t *testing.T, mode string) *credentialProducerAPI {
	t.Helper()
	f := &credentialProducerAPI{objects: make(map[string]*core.Secret), mode: mode}
	f.kube = &kubeClient{namespace: "neon", token: "center-identity"}
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer center-identity" {
			w.WriteHeader(403)
			return
		}
		if r.Method == http.MethodGet {
			secret := f.objects[strings.TrimPrefix(r.URL.Path, f.kube.path("secret", "")+"/")]
			if secret == nil {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(secret)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != f.kube.path("secret", "") {
			w.WriteHeader(405)
			return
		}
		f.posts++
		var proposed core.Secret
		if json.NewDecoder(r.Body).Decode(&proposed) != nil {
			w.WriteHeader(400)
			return
		}
		if f.mode == "429" {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonTooManyRequests, Code: 429})
			return
		}
		// apiserver同名CREATE返回AlreadyExists，不能像普通map赋值覆盖旧
		// immutable对象。并发公开请求必须在真实赢家UID/Data上对账。
		if f.objects[proposed.Name] != nil {
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonAlreadyExists, Code: 409})
			return
		}
		proposed.UID, proposed.ResourceVersion = types.UID("producer-uid-"+strconv.Itoa(f.posts)), "1"
		f.objects[proposed.Name] = proposed.DeepCopy()
		if f.afterPost != nil {
			f.afterPost()
		}
		switch f.mode {
		case "unknown":
			w.WriteHeader(504)
			return
		case "conflict":
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonAlreadyExists, Code: 409})
			return
		case "readback_drift":
			f.objects[proposed.Name].Data["controlSigningSeed"][0] ^= 1
		}
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(&proposed)
	}))
	t.Cleanup(api.Close)
	f.kube.base, f.kube.http = api.URL, api.Client()
	return f
}

func TestDSGACredentialProducerCreateRetryAndUnknownWrite(t *testing.T) {
	for _, mode := range []string{"success", "unknown", "conflict", "429", "readback_drift"} {
		t.Run(mode, func(t *testing.T) {
			p, _, plan := dsgaPlanFixture(t)
			f := newCredentialProducerAPI(t, mode)
			input := dsgaCredentialInput{Password: "original-writer-password"}
			frozen, err := f.kube.prepareDSGACredentials(context.Background(), p, plan, "op_original", input)
			if mode == "429" || mode == "readback_drift" || mode == "unknown" {
				if !errors.Is(err, errDSGACredentialSource) || frozen != (dsgaCreationPlan{}) {
					t.Fatal("failed/unknown write fabricated frozen plan")
				}
			} else if err != nil || frozen.Credentials.validate() != nil {
				t.Fatal("create/409 did not freeze actual source")
			}
			f.mu.Lock()
			posts := f.posts
			secret := f.objects[kubeName(p.EndpointID)+"-credentials"]
			var before *core.Secret
			if secret != nil {
				before = secret.DeepCopy()
			}
			f.mu.Unlock()
			if posts != 1 {
				t.Fatal("POST was transparently retried")
			}
			if mode == "429" || mode == "readback_drift" {
				return
			}
			// 原意图对账应读回同一UID、Data与RV；不论前次响应是否丢失，不再POST。
			again, err := f.kube.prepareDSGACredentials(context.Background(), p, plan, "op_original", input)
			if err != nil {
				t.Fatal("original retry did not reconcile")
			}
			f.mu.Lock()
			after, _ := json.Marshal(f.objects[before.Name])
			previous, _ := json.Marshal(before)
			posts = f.posts
			f.mu.Unlock()
			if posts != 1 || !bytes.Equal(previous, after) || (mode != "unknown" && again != frozen) {
				t.Fatal("retry rotated or rewrote original credential")
			}
			if got, err := f.kube.prepareDSGACredentials(context.Background(), p, again, "op_replacement", input); err == nil || got != (dsgaCreationPlan{}) {
				t.Fatal("different Operation adopted source")
			}
			bad := input
			bad.Password = "different-writer-password"
			if _, err := f.kube.prepareDSGACredentials(context.Background(), p, again, "op_original", bad); err == nil {
				t.Fatal("retry accepted different password")
			}
			f.mu.Lock()
			delete(f.objects, before.Name)
			f.mu.Unlock()
			if _, err := f.kube.prepareDSGACredentials(context.Background(), p, again, "op_original", input); err == nil {
				t.Fatal("frozen UID was recreated")
			}
			f.mu.Lock()
			posts = f.posts
			f.mu.Unlock()
			if posts != 1 {
				t.Fatal("missing frozen source caused POST")
			}
		})
	}
}

func TestDSGACredentialProducerReplicaCopiesFrozenVerifiers(t *testing.T) {
	p, _, plan := dsgaPlanFixture(t)
	f := newCredentialProducerAPI(t, "success")
	sourcePlan, err := f.kube.prepareDSGACredentials(context.Background(), p, plan, "op_writer", dsgaCredentialInput{Password: "writer-source-password"})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	writer := f.objects[kubeName(p.EndpointID)+"-credentials"].DeepCopy()
	f.mu.Unlock()
	sourceEndpoint := p.EndpointID
	p.EndpointID, p.EndpointType = "ep_replica", "read_only"
	plan, err = newDSGACreationPlan(p, dsgaCreationPolicy{Namespace: plan.Namespace, Image: plan.Image, MemorySlotBytes: plan.MemorySlotBytes, Budget: plan.Budget}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	input := dsgaCredentialInput{SourceEndpoint: sourceEndpoint, Source: sourcePlan.Credentials}
	bad := input
	bad.Source.UID = "replaced-writer"
	if _, err := f.kube.prepareDSGACredentials(context.Background(), p, plan, "op_replica", bad); err == nil {
		t.Fatal("copied source without frozen UID")
	}
	if _, err := f.kube.prepareDSGACredentials(context.Background(), p, plan, "op_replica", dsgaCredentialInput{Password: "invalid-replica-password"}); err == nil {
		t.Fatal("replica generated unrelated verifier")
	}
	if _, err := f.kube.prepareDSGACredentials(context.Background(), p, plan, "op_replica", input); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	replica := f.objects[kubeName(p.EndpointID)+"-credentials"].DeepCopy()
	posts := f.posts
	f.mu.Unlock()
	if posts != 2 || !bytes.Equal(writer.Data["applicationVerifier"], replica.Data["applicationVerifier"]) || !bytes.Equal(writer.Data["probeVerifier"], replica.Data["probeVerifier"]) || !bytes.Equal(writer.Data["probeCredentialSeed"], replica.Data["probeCredentialSeed"]) || bytes.Equal(writer.Data["controlSigningSeed"], replica.Data["controlSigningSeed"]) {
		t.Fatal("replica did not inherit SQL identity with separate control key")
	}
}

func TestDSGACredentialProducerEntropyAndBothSCRAMKeys(t *testing.T) {
	password := "original-writer-password"
	if data, err := makeDSGACredentialData(password, dsgaCredentialSnapshot{}, false, bytes.NewReader(make([]byte, 95))); err == nil || data != nil {
		t.Fatal("partial entropy returned credentials")
	}
	data, err := makeDSGACredentialData(password, dsgaCredentialSnapshot{}, false, bytes.NewReader(bytes.Repeat([]byte{7}, 96)))
	if err != nil || len(data) != 4 || !dsgaPasswordMatches(password, string(data["applicationVerifier"])) {
		t.Fatal("canonical credentials rejected")
	}
	verifier := string(data["applicationVerifier"])
	parts := strings.Split(verifier, ":")
	parts[len(parts)-1] = strings.Repeat("A", 43) + "="
	if dsgaPasswordMatches(password, strings.Join(parts, ":")) {
		t.Fatal("corrupt ServerKey accepted by stored-key-only comparison")
	}
	for _, invalid := range []string{"short", strings.Repeat("a", 257), "valid-length\x00password", "valid-length\xffpassword", "valid-length\u00adpassword", "ＡＢＣＤＥＦＧＨＩＪＫＬ"} {
		if _, ok := dsgaPassword(invalid); ok {
			t.Fatal("invalid or client-incompatible password accepted")
		}
	}
}

func TestDSGACredentialProducerNativePGXAndLibpqSCRAM(t *testing.T) {
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
	// 实际验证新verifier能被两种客户端接受，不把纯哈希自测当认证成功。
	for name, password := range map[string]string{"ascii": "real-native-password", "chinese": "真实数据库口令校验", "non_ascii_space": "native\u00a0password", "composed": "native-e\u0301-password"} {
		t.Run(name, func(t *testing.T) {
			data, err := makeDSGACredentialData(password, dsgaCredentialSnapshot{}, false, bytes.NewReader(bytes.Repeat([]byte{9}, 96)))
			if err != nil {
				t.Fatal("test password unexpectedly incompatible")
			}
			role := newID("dsga_cred_")
			if _, err := admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN PASSWORD "+pgLiteral(string(data["applicationVerifier"]))); err != nil {
				t.Fatal("native CREATE ROLE failed")
			}
			defer func() {
				if _, err := admin.Exec(ctx, "DROP ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
					t.Error("owned role cleanup failed")
				}
			}()
			config, err := pgx.ParseConfig(url)
			if err != nil {
				t.Fatal(err)
			}
			config.User, config.Password = role, password
			conn, err := pgx.ConnectConfig(ctx, config)
			if err != nil {
				t.Fatal("native pgx SCRAM failed")
			}
			var actual string
			err = conn.QueryRow(ctx, "SELECT current_user").Scan(&actual)
			_ = conn.Close(ctx)
			if err != nil || actual != role {
				t.Fatal("pgx did not authenticate intended role")
			}
			cmd := exec.CommandContext(ctx, psql, "-X", "-w", "-At", "-c", "SELECT current_user")
			// 使用独立连接变量替换管理员身份；口令只在子进程环境，不进argv/日志。
			cmd.Env = append(os.Environ(), "PGHOST="+config.Host, "PGPORT="+strconv.Itoa(int(config.Port)), "PGDATABASE="+config.Database, "PGUSER="+role, "PGPASSWORD="+password, "PGSSLMODE=disable")
			out, err := cmd.Output()
			if err != nil || strings.TrimSpace(string(out)) != role {
				t.Fatal("native libpq SCRAM failed")
			}
		})
	}
}
