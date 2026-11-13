package control

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// 真实隔离PostgreSQL验证v5前向迁移：新reservation必须冻结Runner Pod硬预算；
// 旧v3计划在迁移前已存在时仍可按原计划绑定来源，不得被迁移重写或升级。
func TestDSGAReservationVM5RunnerPodHardBudgetForwardMigration(t *testing.T) {
	f := newDSGAProjectFixture(t)

	makePlan := func(vm dsgaVMProfile) (createPayload, dsgaCreationPlan) {
		t.Helper()
		payload, policy, _ := dsgaPlanFixture(t)
		policy.VM = vm
		plan, err := newDSGACreationPlan(payload, policy, time.Now())
		if err != nil {
			t.Fatal("build frozen fixture plan:", err)
		}
		if err := plan.validate(payload); err != nil {
			t.Fatal("fixture plan did not pass Go validation:", err)
		}
		return payload, plan
	}

	insert := func(name, operation string, payload createPayload, plan dsgaCreationPlan) error {
		t.Helper()
		draft := dsgaCreationDraft{1, name, "main", "test-region", payload, plan}
		raw, err := json.Marshal(draft)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.s.db.Exec(f.ctx, "INSERT INTO dsga_creation_reservations (actor_id,key_hash,request_hash,scope,org_id,base_action,operation_id,draft) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)",
			"creation_admin", keyHash("v5-reservation-"+name), "h1:"+strings.Repeat("a", 64),
			"POST:/organizations/org_creation/projects", "org_creation", "create_project", operation, raw)
		return err
	}

	// 先按已发布v4数据库守卫创建一条历史v3 reservation，模拟升级前已接受的原始计划。
	legacyGuard, err := migrations.ReadFile("migrations/900000070_dsga_automatic_supply.sql")
	if err != nil {
		t.Fatal(err)
	}
	legacySQL := string(legacyGuard)
	start := strings.Index(legacySQL, "CREATE OR REPLACE FUNCTION guard_dsga_creation_reservation()")
	end := strings.Index(legacySQL, "\n\n-- 原创建操作的外部对象回执")
	if start < 0 || end <= start {
		t.Fatal("published v4 reservation guard is missing")
	}
	f.exec(legacySQL[start:end])

	v3Payload, v3Plan := makePlan(dsgaVMProfileFixture())
	if err := insert("historical-v3", "op_dsga_v3_historical", v3Payload, v3Plan); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			t.Fatalf("v4 guard rejected historical v3 plan: code=%s position=%d internal=%q where=%q detail=%q message=%q",
				pgErr.Code, pgErr.Position, pgErr.InternalQuery, pgErr.Where, pgErr.Detail, pgErr.Message)
		}
		t.Fatal("v4 guard did not accept the historical v3 plan:", err)
	}
	var beforeRow, beforeXmin string
	if err := f.s.db.QueryRow(f.ctx,
		"SELECT row_to_json(r)::text,xmin::text FROM dsga_creation_reservations r WHERE operation_id=$1",
		"op_dsga_v3_historical").Scan(&beforeRow, &beforeXmin); err != nil {
		t.Fatal(err)
	}

	// 只执行新迁移中的CREATE OR REPLACE FUNCTION；不重建表、不回写历史数据。
	forward, err := migrations.ReadFile("migrations/900000072_dsga_reservation_vm_v5_runner_budget.sql")
	if err != nil {
		t.Fatal(err)
	}
	f.exec(string(forward))
	var afterRow, afterXmin string
	if err := f.s.db.QueryRow(f.ctx,
		"SELECT row_to_json(r)::text,xmin::text FROM dsga_creation_reservations r WHERE operation_id=$1",
		"op_dsga_v3_historical").Scan(&afterRow, &afterXmin); err != nil {
		t.Fatal(err)
	}
	if beforeRow != afterRow || beforeXmin != afterXmin {
		t.Fatal("v5 forward migration rewrote an existing v3 reservation")
	}
	ref, _ := json.Marshal(dsgaCredentialReference{1, "original-v3-credentials", strings.Repeat("c", 64)})
	if _, err := f.s.db.Exec(f.ctx,
		"UPDATE dsga_creation_reservations SET credential_reference=$1::jsonb WHERE operation_id=$2",
		ref, "op_dsga_v3_historical"); err != nil {
		t.Fatal("historical v3 plan could not make its original credential-source transition:", err)
	}

	// 新建v3已不再符合当前发行合同；新创建必须使用显式v5。
	if err := insert("new-v3-rejected", "op_dsga_v3_new", v3Payload, v3Plan); dsgaReservationPGCode(err) != "23514" {
		t.Fatal("new v3 reservation bypassed the v5-only creation gate:", err)
	}

	v5Payload, v5Plan := makePlan(dsgaVMProfileV5Fixture())
	if v5Plan.VM.RunnerPodResources == nil {
		t.Fatal("v5 fixture is missing its frozen Runner Pod resource budget")
	}
	if err := insert("v5-valid", "op_dsga_v5_valid", v5Payload, v5Plan); err != nil {
		t.Fatal("valid v5 Runner Pod hard budget was rejected:", err)
	}

	// 每种失配都走真实PostgreSQL BEFORE触发器，而非仅调用Go校验器。
	invalid := map[string]func(*dsgaCreationPlan){
		"missing_runner_budget": func(p *dsgaCreationPlan) {
			p.VM.RunnerPodResources = nil
		},
		"cpu_request_below_guest_max": func(p *dsgaCreationPlan) {
			p.VM.RunnerPodResources.CPURequestMilli = 1000
		},
		"cpu_limit_below_request": func(p *dsgaCreationPlan) {
			p.VM.RunnerPodResources.CPULimitMilli = 1500
		},
		"cpu_limit_above_four_times_guest_max": func(p *dsgaCreationPlan) {
			p.VM.RunnerPodResources.CPULimitMilli = 8001
		},
		"runner_memory_below_guest_max_plus_host_reserve": func(p *dsgaCreationPlan) {
			p.VM.RunnerPodResources.MemoryRequestBytes = 6 << 30
			p.VM.RunnerPodResources.MemoryLimitBytes = 6 << 30
		},
		"runner_memory_request_limit_mismatch": func(p *dsgaCreationPlan) {
			p.VM.RunnerPodResources.MemoryLimitBytes = 8 << 30
		},
		"missing_node_placement": func(p *dsgaCreationPlan) {
			p.VM.Placement = nil
		},
		"missing_guest_resource_limits": func(p *dsgaCreationPlan) {
			p.VM.ResourceLimits = nil
		},
	}
	for name, mutate := range invalid {
		t.Run(name, func(t *testing.T) {
			candidate := v5Plan
			candidate.VM = v5Plan.VM.clone()
			mutate(&candidate)
			err := insert("invalid-"+name, "op_dsga_v5_invalid_"+strings.ReplaceAll(name, "-", "_"), v5Payload, candidate)
			if code := dsgaReservationPGCode(err); code != "23514" {
				t.Fatalf("invalid v5 reservation was not rejected by SQL guard: SQLSTATE=%q err=%v", code, err)
			}
		})
	}
}
