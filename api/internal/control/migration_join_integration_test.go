package control

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 创建真正的旧分支schema及仅数字登记的历史，再调用实际migrate升级。不能用
// 最新schema删表模拟旧版本，也不允许改写058或伪造两份都曾执行的历史记录。
func migration058BranchFixture(t *testing.T, source string) (context.Context, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("NEON_V2_TEST_DATABASE_URL not set")
	}
	if !regexp.MustCompile(`^v2_migration_[a-z0-9_]+$`).MatchString(os.Getenv("NEON_V2_TEST_SCHEMA")) {
		t.Fatal("a dedicated v2_migration_* schema is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	schema := "v2_migration_join_" + stableSuffix("test", "join", newID("schema_"))
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	admin.Close()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `CREATE TABLE schema_migrations(version integer PRIMARY KEY,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		n, err := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
		if err != nil {
			t.Fatal(err)
		}
		if n > 57 {
			continue
		}
		raw, err := migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, string(raw), pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version)VALUES($1)`, n); err != nil {
			t.Fatal(err)
		}
	}
	if source != "fresh" {
		raw, err := migrations.ReadFile("migrations/" + source)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, string(raw), pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version)VALUES(58)`); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return ctx, pool
}

func TestMigration058JoinUpgradesBothReleasedBranches(t *testing.T) {
	for _, source := range []string{"fresh", "058_dsga_creation_identity.sql", "058_catalog_current_observations.sql"} {
		t.Run(source, func(t *testing.T) {
			ctx, pool := migration058BranchFixture(t, source)
			auditQuery := migration058SeedOriginalAudit(t, ctx, pool, source)
			var auditBefore string
			if auditQuery != "" {
				if err := pool.QueryRow(ctx, auditQuery).Scan(&auditBefore); err != nil {
					t.Fatal(err)
				}
			}
			// 保存原对象OID，证明upgrade不DROP重建已存在的审计/UID表；旧历史行不改写。
			table := "dsga_creation_bindings"
			if source == "058_catalog_current_observations.sql" {
				table = "catalog_current_observations"
			}
			var before *uint32
			var historyBefore string
			if err := pool.QueryRow(ctx, `SELECT to_regclass($1)::oid`, table).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT coalesce(string_agg(version::text||':'||applied_at::text,',' ORDER BY version),'') FROM schema_migrations`).Scan(&historyBefore); err != nil {
				t.Fatal(err)
			}
			for run := 0; run < 2; run++ {
				if err := migrate(ctx, pool); err != nil {
					t.Fatalf("real upgrade from %s: %v", source, err)
				}
			}
			var after uint32
			if err := pool.QueryRow(ctx, `SELECT to_regclass($1)::oid`, table).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if before != nil && *before != after {
				t.Fatal("upgrade replaced an original identity table")
			}
			for _, name := range []string{"dsga_creation_bindings", "catalog_current_observations"} {
				var exists bool
				if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil || !exists {
					t.Fatalf("missing joined schema %s: %v", name, err)
				}
			}
			var count int
			var historyAfter string
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version=900000059`).Scan(&count); err != nil || count != 1 {
				t.Fatal("join witness missing", err)
			}
			cutoff := 58
			if source == "fresh" {
				cutoff = 57
			}
			if err := pool.QueryRow(ctx, `SELECT string_agg(version::text||':'||applied_at::text,',' ORDER BY version) FROM schema_migrations WHERE version<=$1`, cutoff).Scan(&historyAfter); err != nil {
				t.Fatal(err)
			}
			if historyAfter != historyBefore {
				t.Fatal("upgrade rewrote original numeric migration history")
			}
			if auditQuery != "" {
				var auditAfter string
				if err := pool.QueryRow(ctx, auditQuery).Scan(&auditAfter); err != nil || auditAfter != auditBefore {
					t.Fatal("upgrade changed original audit/UID row or xmin", err)
				}
			}
		})
	}
}

// 种入两种来源各自真实FK/不可变审计行，比较整行及xmin；只比较表存在或OID
// 不足以证明升级保留原事实。值均为隔离schema内测试常量，不使用线上业务对象。
func migration058SeedOriginalAudit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, source string) string {
	t.Helper()
	if source == "fresh" {
		return ""
	}
	base := `INSERT INTO projects(id,org_id,name,tenant_id,region_id,postgres_version,state,source,created_at,updated_at)
 VALUES('original_project','local','original',repeat('a',32),'test',17,'ready','managed',now(),now());
 INSERT INTO branches(id,project_id,name,timeline_id,state,created_at)
 VALUES('original_branch','original_project','main',repeat('b',32),'ready',now());`
	table := "dsga_creation_bindings"
	if source == "058_dsga_creation_identity.sql" {
		base += `INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,created_at,updated_at)
 VALUES('original_endpoint','original_project','original_branch','original','neonvm','original','original','provisioning','app_owner','postgres',now(),now());
 INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,lease_owner,lease_expires_at)
 VALUES('original_operation','original_project','endpoint','original_endpoint','create_endpoint_dsga_v1','running','test','test',
 '{"project_id":"original_project","branch_id":"original_branch","endpoint_id":"original_endpoint","dsga_creation":{"protocol_version":1,"postgres_version":17}}',
 'original_worker',clock_timestamp()+interval '60 seconds');
 INSERT INTO dsga_creation_bindings(operation_id,namespace,vm_name,incarnation,compute_instance_id,intent_digest,vm_uid,config_uid)
 VALUES('original_operation','neon','original-vm',repeat('a',32),'original_compute',decode(repeat('ab',32),'hex'),'original-vm-uid','original-config-uid');`
	} else {
		table = "catalog_current_observations"
		base += `INSERT INTO users(id,username,password_hash,role) VALUES('original_user','original_user','test-only','owner');
 INSERT INTO catalog_identity_baselines(id,project_id,branch_id,kind,name,object_oid,intent_version,tenant_id,timeline_id,expectation,marker_sha256,captured_by)
 VALUES('original_baseline','original_project','original_branch','database','app',1001,1,repeat('a',32),repeat('b',32),'{}',repeat('c',64),'original_user');
 INSERT INTO catalog_object_bindings(id,identity_id,project_id,branch_id,kind,name,object_oid,intent_version,tenant_id,timeline_id,state,owner_oid,source_type,source_id)
 VALUES('original_binding','original_identity','original_project','original_branch','database','app',1001,1,repeat('a',32),repeat('b',32),'ready',10,'current_observation','original_observation');
 INSERT INTO catalog_current_observations(id,binding_id,baseline_id,project_id,branch_id,proof)
 VALUES('original_observation','original_binding','original_baseline','original_project','original_branch','{"marker":"preserved"}');`
	}
	if _, err := pool.Exec(ctx, base, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`SELECT row_to_json(original)::text||'|'||xmin::text FROM %s original`, pgx.Identifier{table}.Sanitize())
}

func TestMigration058JoinRejectsPartialGuardsAtomically(t *testing.T) {
	for _, source := range []string{"058_dsga_creation_identity.sql", "058_catalog_current_observations.sql"} {
		t.Run(source, func(t *testing.T) {
			ctx, pool := migration058BranchFixture(t, source)
			table, trigger, missing := "dsga_creation_bindings", "dsga_creation_binding_immutable", "catalog_current_observations"
			if source == "058_catalog_current_observations.sql" {
				table, trigger, missing = "catalog_current_observations", "preserve_catalog_current_observation", "dsga_creation_bindings"
			}
			if _, err := pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s DISABLE TRIGGER %s`, pgx.Identifier{table}.Sanitize(), pgx.Identifier{trigger}.Sanitize())); err != nil {
				t.Fatal(err)
			}
			if err := migrate(ctx, pool); err == nil {
				t.Fatal("partial original guards accepted as completed migration")
			}
			var exists bool
			var count int
			if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, missing).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version=900000059`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if exists || count != 0 {
				t.Fatal("failed join left partial DDL or false successful witness")
			}
		})
	}
}
