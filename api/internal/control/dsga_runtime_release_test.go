package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func dsgaReleaseCatalogFixture(t *testing.T) dsgaRuntimeCatalog {
	t.Helper()
	_, policy, _ := dsgaPlanFixture(t)
	policy.VM = dsgaVMProfileV5Fixture()
	policy.NativeBootstrapVersion = 3
	catalog := dsgaRuntimeCatalog{ProtocolVersion: 1}
	for _, version := range []uint32{17, 18} {
		release := policy
		release.PostgresVersion = version
		release.Image = fmt.Sprintf("registry.test/neon/pg%d@sha256:%s", version, strings.Repeat("a", 64))
		catalog.Releases = append(catalog.Releases, dsgaProjectPolicy{1, "test-region", createBounds{500, 2000, 1024, 4096}, release})
	}
	return catalog
}

func setDSGACatalog(t *testing.T, catalog dsgaRuntimeCatalog) {
	t.Helper()
	raw, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEON_DSGA_CREATION_POLICY", "")
	t.Setenv("NEON_DSGA_RUNTIME_CATALOG", string(raw))
}

func TestDSGARuntimeCatalogRejectsAmbiguousOrUnsafeConfiguration(t *testing.T) {
	for name, mutate := range map[string]func(*dsgaRuntimeCatalog){
		"unknown_protocol": func(c *dsgaRuntimeCatalog) { c.ProtocolVersion = 2 },
		"unknown_pg":       func(c *dsgaRuntimeCatalog) { c.Releases[1].Creation.PostgresVersion = 19 },
		"implicit_pg":      func(c *dsgaRuntimeCatalog) { c.Releases[1].Creation.PostgresVersion = 0 },
		"lease_contract":   func(c *dsgaRuntimeCatalog) { c.Releases[1].Creation.NativeBootstrapVersion = 2 },
		"duplicate":        func(c *dsgaRuntimeCatalog) { c.Releases = append(c.Releases, c.Releases[0]) },
		"mutable_image":    func(c *dsgaRuntimeCatalog) { c.Releases[1].Creation.Image = "registry.test/pg18:latest" },
		"wrong_namespace":  func(c *dsgaRuntimeCatalog) { c.Releases[1].Creation.Namespace = "other" },
		"bad_budget":       func(c *dsgaRuntimeCatalog) { c.Releases[1].Creation.Budget.MinimumGuestMemoryBytes = 1 },
		"pod_cpu_under_guest_max": func(c *dsgaRuntimeCatalog) {
			c.Releases[1].Creation.VM.RunnerPodResources.CPURequestMilli = 1000
		},
		"pod_memory_under_guest_max": func(c *dsgaRuntimeCatalog) {
			c.Releases[1].Creation.VM.RunnerPodResources.MemoryRequestBytes = 6 << 30
			c.Releases[1].Creation.VM.RunnerPodResources.MemoryLimitBytes = 6 << 30
		},
		"pod_memory_request_limit_mismatch": func(c *dsgaRuntimeCatalog) {
			c.Releases[1].Creation.VM.RunnerPodResources.MemoryLimitBytes = 8 << 30
		},
	} {
		t.Run(name, func(t *testing.T) {
			catalog := dsgaReleaseCatalogFixture(t)
			mutate(&catalog)
			setDSGACatalog(t, catalog)
			// 即使坏的是另一版本，也不能从损坏的发行目录接受新操作。
			if _, err := selectDSGAProjectPolicy("neon", 17, "test-region"); !errors.Is(err, errDSGACreationPlan) {
				t.Fatalf("unsafe catalog accepted: %v", err)
			}
		})
	}
	t.Run("conflicting_configuration", func(t *testing.T) {
		setDSGACatalog(t, dsgaReleaseCatalogFixture(t))
		t.Setenv("NEON_DSGA_CREATION_POLICY", "{}")
		if _, err := readDSGARuntimePolicies("neon"); !errors.Is(err, errDSGACreationPlan) {
			t.Fatal("conflicting source accepted")
		}
	})
	t.Run("region_selection", func(t *testing.T) {
		c := dsgaReleaseCatalogFixture(t)
		other := c.Releases[0]
		other.Region = "second-region"
		c.Releases = append(c.Releases, other)
		setDSGACatalog(t, c)
		if _, err := selectDSGAProjectPolicy("neon", 17, ""); !errors.Is(err, errDSGARuntimeUnavailable) {
			t.Fatal("ambiguous region picked arbitrarily")
		}
		p, err := selectDSGAProjectPolicy("neon", 17, "second-region")
		if err != nil || p.Region != "second-region" {
			t.Fatal("explicit region rejected", err)
		}
	})
}

// 公共HTTP准入→真实元数据库迁移/不可变计划→同一VM/存储投影。
// 外部Kube在此仍是协议夹具，不将本测试计为真实Compute或内存E2E。
func TestDSGARuntimeVersionsShareCreationAndImmutableRecovery(t *testing.T) {
	for _, version := range []uint32{17, 18} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			f := newDSGAProjectFixture(t)
			setDSGACatalog(t, dsgaReleaseCatalogFixture(t))
			f.body.PostgresVersion = int(version)
			key := "multiversion-original-creation"
			if w := f.request("creation_admin", key, f.body); w.Code != 202 {
				t.Fatalf("acceptance: %d %s", w.Code, w.Body.String())
			}
			op, draft, xmin := f.reservation(key)
			f.exec(`UPDATE operations SET state='running',lease_owner='version-worker',lease_expires_at=clock_timestamp()+interval '60 seconds' WHERE id=$1`, op)
			binding, err := f.s.loadDSGACreationBinding(f.ctx, op, "version-worker")
			if err != nil {
				t.Fatal(err)
			}
			plan, err := binding.creationPlan(f.ctx)
			if err != nil || plan.PostgresVersion != version {
				t.Fatal("frozen version differs", err)
			}
			vm, err := renderDSGAVM(binding.intent.createPayload, plan)
			if err != nil || vm.Spec.Guest.Env[0].Value != fmt.Sprint(version) || vm.Spec.Guest.RootDisk.Image != plan.Image {
				t.Fatal("VM projection differs", err)
			}
			request, err := binding.timelineRequest(plan)
			if err != nil || request.PostgresVersion != version {
				t.Fatal("timeline version differs", err)
			}
			if _, err = binding.tenantIdentity(f.ctx, true); err != nil {
				t.Fatal("shared tenant guard rejected", err)
			}
			if _, err = binding.timelineIdentity(f.ctx, request, false); err != nil {
				t.Fatal("shared timeline guard rejected", err)
			}
			if confirmed, err := binding.timelineIdentity(f.ctx, request, true); err != nil || !confirmed {
				t.Fatal("timeline confirmation failed", err)
			}
			// 真实分片返回另一PG版本时仍拒绝，不允许“支持两个版本”变成忽略身份。
			shard := map[string]any{"tenant_id": binding.intent.TenantID, "timeline_id": request.TimelineID, "pg_version": version, "state": "Active", "ancestor_timeline_id": nil, "ancestor_lsn": nil}
			fact := map[string]any{"shards": []any{shard}}
			if err := validateDSGATimelineAggregate(fact, binding.intent.TenantID, request.TimelineID, version, "", "", true); err != nil {
				t.Fatal(err)
			}
			shard["pg_version"] = uint32(35) - version
			if err := validateDSGATimelineAggregate(fact, binding.intent.TenantID, request.TimelineID, version, "", "", true); err == nil {
				t.Fatal("other PG version accepted")
			}
			// 接受后的版本/来源已冻结，移走发行目录也不会迁移/重建旧原意图。
			t.Setenv("NEON_DSGA_RUNTIME_CATALOG", "invalid-after-acceptance")
			if w := f.request("creation_admin", key, f.body); w.Code != 202 {
				t.Fatal("replay consulted new catalog", w.Code)
			}
			afterOp, afterDraft, afterXmin := f.reservation(key)
			if afterOp != op || afterDraft != draft || afterXmin != xmin {
				t.Fatal("replay changed original reservation")
			}
			recovered, err := f.s.loadDSGACreationBinding(f.ctx, op, "version-worker")
			if err != nil {
				t.Fatal(err)
			}
			same, err := recovered.creationPlan(f.ctx)
			if err != nil || same.PostgresVersion != version || same.Image != plan.Image {
				t.Fatal("recovery changed runtime", err)
			}
			changed := f.body
			changed.PostgresVersion = 35 - int(version)
			if w := f.request("creation_admin", key, changed); w.Code != 409 {
				t.Fatal("same key changed PG version", w.Code)
			}
			// 写者不能更新不可变Operation，也不能让PG18资源冒充PG17原计划。
			_, err = f.s.db.Exec(f.ctx, `UPDATE operations SET payload=jsonb_set(payload,'{dsga_creation,postgres_version}',to_jsonb($2::integer)) WHERE id=$1`, op, 35-int(version))
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
				t.Fatal("database allowed version rewrite", err)
			}
			f.exec(`UPDATE projects SET postgres_version=$2 WHERE id=$1`, binding.intent.ProjectID, 35-int(version))
			if _, err = f.s.loadDSGACreationBinding(f.ctx, op, "version-worker"); !errors.Is(err, errDSGACreationIdentity) {
				t.Fatal("project/plan mismatch accepted", err)
			}
		})
	}
}

func TestDSGARuntimeUnavailableDoesNotAllocateCredentials(t *testing.T) {
	f := newDSGAProjectFixture(t)
	f.body.PostgresVersion = 18 // 原单版本配置仍仅支持17，不能借17镜像创建18。
	if w := f.request("creation_admin", "unavailable-runtime", f.body); w.Code != 422 {
		t.Fatal("unsupported release accepted", w.Code)
	}
	f.body.PostgresVersion = 4294967314 // uint32转换后为18，也不能接受。
	if w := f.request("creation_admin", "overflow-runtime", f.body); w.Code != 422 {
		t.Fatal("overflow release accepted", w.Code)
	}
	f.credentials.mu.Lock()
	defer f.credentials.mu.Unlock()
	if f.credentials.posts != 0 {
		t.Fatal("unavailable release allocated credentials")
	}
	var count int
	if err := f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM dsga_creation_reservations`).Scan(&count); err != nil || count != 0 {
		t.Fatal("unavailable release reserved quota", err)
	}
}

func TestDSGARuntimeCapabilitiesPublishOnlyConfiguredProductFields(t *testing.T) {
	setDSGACatalog(t, dsgaReleaseCatalogFixture(t))
	t.Setenv("NEON_V2_CREATE_ENABLED", "true")
	t.Setenv("NEON_DSGA_CREATE_ENABLED", "true")
	s := &server{kube: &kubeClient{namespace: "neon"}, idempotencyKey: make([]byte, 32)}
	options := s.postgresRuntimeOptions()
	if len(options) != 2 || options[0].PostgresVersion != 17 || options[1].PostgresVersion != 18 || !options[1].CreateEnabled || options[1].MemoryScaling != "vm_and_postgres" {
		t.Fatal("configured product versions missing")
	}
	raw, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"sha256:", "runner_image", "service_uid", "node_selector", "secret", "registry.test"} {
		if strings.Contains(string(raw), private) {
			t.Fatal("platform detail escaped to product capabilities", private)
		}
	}
	t.Setenv("NEON_DSGA_CREATE_ENABLED", "false")
	options = s.postgresRuntimeOptions()
	if options[0].CreateEnabled || options[1].CreateEnabled {
		t.Fatal("runtime creation gate bypassed")
	}
	t.Setenv("NEON_DSGA_RUNTIME_CATALOG", "invalid")
	if len(s.postgresRuntimeOptions()) != 0 {
		t.Fatal("unconfirmed runtime advertised")
	}
}

func TestDSGARuntimeForwardMigrationKeepsOriginalPG17Intent(t *testing.T) {
	f := newDSGAProjectFixture(t)
	key := "historical-pg17-upgrade"
	if w := f.request("creation_admin", key, f.body); w.Code != 202 {
		t.Fatal("original project missing", w.Code)
	}
	op, draft, xmin := f.reservation(key)
	f.exec(`UPDATE operation_steps SET name='create_timeline_pg17' WHERE operation_id=$1 AND ordinal=1`, op)
	// 恢复确切旧函数及迁移标记，模拟已有PG17原意图的前向升级，不改历史文件。
	raw, err := os.ReadFile("migrations/058_dsga_creation_identity.sql")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	start := strings.Index(source, "CREATE OR REPLACE FUNCTION guard_operation_lifecycle()")
	end := strings.Index(source[start:], "END $$;") + start + len("END $$;")
	f.exec(source[start:end])
	for _, name := range []string{"900000064_dsga_creation_tenants.sql", "900000065_dsga_creation_timelines.sql", "900000067_dsga_reservation_vm_v3_placement.sql"} {
		raw, err := os.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		source := string(raw)
		start := strings.Index(source, "CREATE ")
		if !strings.HasPrefix(source[start:], "CREATE OR REPLACE FUNCTION") {
			start = strings.Index(source, "CREATE FUNCTION")
		}
		end := strings.Index(source[start:], "END $$;") + start + len("END $$;")
		f.exec(strings.Replace(source[start:end], "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION", 1))
	}
	f.exec(`DELETE FROM schema_migrations WHERE version=900000068`)
	var payloadBefore, payloadAfter, opXminBefore, opXminAfter string
	if err := f.s.db.QueryRow(f.ctx, `SELECT payload::text,xmin::text FROM operations WHERE id=$1`, op).Scan(&payloadBefore, &opXminBefore); err != nil {
		t.Fatal(err)
	}
	if err := migrate(f.ctx, f.s.db); err != nil {
		t.Fatal(err)
	}
	if err := migrate(f.ctx, f.s.db); err != nil {
		t.Fatal(err)
	}
	_, afterDraft, afterXmin := f.reservation(key)
	if err := f.s.db.QueryRow(f.ctx, `SELECT payload::text,xmin::text FROM operations WHERE id=$1`, op).Scan(&payloadAfter, &opXminAfter); err != nil {
		t.Fatal(err)
	}
	if draft != afterDraft || xmin != afterXmin || payloadBefore != payloadAfter || opXminBefore != opXminAfter {
		t.Fatal("migration rewrote original plan or source")
	}
	f.exec(`UPDATE operations SET state='running',lease_owner='upgrade-worker',lease_expires_at=clock_timestamp()+interval '60 seconds' WHERE id=$1`, op)
	b, err := f.s.loadDSGACreationBinding(f.ctx, op, "upgrade-worker")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := b.creationPlan(f.ctx)
	if err != nil || plan.PostgresVersion != 17 {
		t.Fatal("old intent no longer recoverable", err)
	}
}
