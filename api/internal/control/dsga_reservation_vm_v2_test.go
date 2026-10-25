package control

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// 使用真实PG和已发布旧函数造出升级前的v1原意图，再只运行前向DDL。
// 比较整行及xmin：格式修复不得重写原计划、随机身份、凭据引用或接受状态。
// 该测试不调用K8s，不将数据库格式接受解释为预算已落实或VM启动成功。
func TestDSGAReservationVM2ForwardMigrationPreservesOriginalAndRejectsPartial(t *testing.T) {
	f := newDSGAProjectFixture(t)
	old, err := migrations.ReadFile("migrations/900000061_dsga_creation_reservations.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(old)
	start := strings.Index(text, "CREATE FUNCTION guard_dsga_creation_reservation()")
	end := strings.Index(text, "CREATE TRIGGER dsga_creation_reservation_guard")
	if start < 0 || end <= start {
		t.Fatal("published guard is missing")
	}
	f.exec(strings.Replace(text[start:end], "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION", 1))
	p, _, plan := dsgaPlanFixture(t)
	plan.VM = dsgaVMProfileFixture()
	plan.VM.ProtocolVersion, plan.VM.ResourceLimits = 1, nil
	draft := dsgaCreationDraft{1, "historical", "main", "test-region", p, plan}
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO dsga_creation_reservations(actor_id,key_hash,request_hash,scope,org_id,base_action,operation_id,draft)
 VALUES('creation_admin',$1,$2,'POST:/organizations/org_creation/projects','org_creation','create_project',$3,$4)`
	f.exec(insert, keyHash("before-vm2-migration"), "h1:"+strings.Repeat("a", 64), "op_before_vm2", raw)
	read := func() (string, string) {
		t.Helper()
		var row, xmin string
		if err := f.s.db.QueryRow(f.ctx, `SELECT row_to_json(r)::text,xmin::text FROM dsga_creation_reservations r WHERE operation_id='op_before_vm2'`).Scan(&row, &xmin); err != nil {
			t.Fatal(err)
		}
		return row, xmin
	}
	before, xmin := read()
	forward, err := migrations.ReadFile("migrations/900000066_dsga_reservation_vm_v2.sql")
	if err != nil {
		t.Fatal(err)
	}
	f.exec(string(forward))
	if after, afterXmin := read(); after != before || afterXmin != xmin {
		t.Fatal("forward migration rewrote original reservation")
	}
	// 不能修改旧原意图来适配新生产者；v1解码兼容只供读取，原Go恢复拒绝缺预算。
	if _, err := f.s.db.Exec(f.ctx, `UPDATE dsga_creation_reservations SET draft=jsonb_set(draft,'{plan,vm,protocol_version}','2') WHERE operation_id='op_before_vm2'`); dsgaReservationPGCode(err) != "23514" {
		t.Fatal("historical intent was silently upgraded")
	}
	if _, err := f.s.db.Exec(f.ctx, insert, keyHash("new-vm1"), "h1:"+strings.Repeat("b", 64), "op_new_vm1", raw); dsgaReservationPGCode(err) != "23514" {
		t.Fatal("new v1 reservation bypassed v2 producer contract")
	}
	plan.VM = dsgaVMProfileFixture()
	draft.Plan = plan
	draft.ResourceName = "current"
	raw, err = json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(insert, keyHash("new-vm2"), "h1:"+strings.Repeat("c", 64), "op_new_vm2", raw)
	// JSONB缺失/null/错类型和未知协议均直接碰真实数据库守卫，不由Go提前挡掉。
	for _, mutation := range []string{
		`draft #- '{plan,vm,resource_limits}'`,
		`jsonb_set(draft,'{plan,vm,resource_limits}','null')`,
		`jsonb_set(draft,'{plan,vm,resource_limits}','[]')`,
		`jsonb_set(draft,'{plan,vm,protocol_version}','3')`,
		`draft #- '{plan,vm,protocol_version}'`,
	} {
		var bad []byte
		if err := f.s.db.QueryRow(f.ctx, "SELECT ("+mutation+")::text FROM dsga_creation_reservations WHERE operation_id='op_new_vm2'").Scan(&bad); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.db.Exec(f.ctx, insert, keyHash(mutation), "h1:"+strings.Repeat("d", 64), "op_bad_vm2", bad); dsgaReservationPGCode(err) != "23514" {
			t.Fatal("partial or unknown v2 reservation accepted", mutation, err)
		}
	}
	if after, afterXmin := read(); after != before || afterXmin != xmin {
		t.Fatal("new reservations changed historical original")
	}
}

func dsgaReservationPGCode(err error) string {
	if e, ok := err.(*pgconn.PgError); ok {
		return e.Code
	}
	return ""
}
