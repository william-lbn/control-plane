package control

import (
	"encoding/json"
	"strings"
	"testing"
)

// 真实隔离PG只验证前向格式/不可变意图；不把DDL成功解释成节点授权或VM完成。
func TestDSGAReservationVM3ForwardMigrationPreservesV1V2AndRequiresPlacement(t *testing.T) {
	f := newDSGAProjectFixture(t)
	old, err := migrations.ReadFile("migrations/900000061_dsga_creation_reservations.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(old)
	start := strings.Index(text, "CREATE FUNCTION guard_dsga_creation_reservation()")
	end := strings.Index(text, "CREATE TRIGGER dsga_creation_reservation_guard")
	if start < 0 || end <= start {
		t.Fatal("published v1 guard missing")
	}
	f.exec(strings.Replace(text[start:end], "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION", 1))
	payload, _, plan := dsgaPlanFixture(t)
	plan.VM = dsgaVMProfileFixture()
	plan.VM.ProtocolVersion = 1
	plan.VM.ResourceLimits = nil
	plan.VM.Placement = nil
	draft := dsgaCreationDraft{1, "historical", "main", "test-region", payload, plan}
	marshal := func(v dsgaCreationDraft) []byte {
		t.Helper()
		raw, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		return raw
	}
	insert := "INSERT INTO dsga_creation_reservations(actor_id,key_hash,request_hash,scope,org_id,base_action,operation_id,draft) VALUES('creation_admin',$1,$2,'POST:/organizations/org_creation/projects','org_creation','create_project',$3,$4)"
	add := func(key, op string, raw []byte) {
		t.Helper()
		f.exec(insert, keyHash(key), "h1:"+strings.Repeat("a", 64), op, raw)
	}
	add("vm3-old-v1", "op_vm3_old_v1", marshal(draft))
	forward2, e := migrations.ReadFile("migrations/900000066_dsga_reservation_vm_v2.sql")
	if e != nil {
		t.Fatal(e)
	}
	f.exec(string(forward2))
	plan.VM = dsgaVMProfileFixture()
	plan.VM.ProtocolVersion = 2
	plan.VM.Placement = nil
	draft.Plan = plan
	draft.ResourceName = "historical-v2"
	add("vm3-old-v2", "op_vm3_old_v2", marshal(draft))
	read := func(op string) (string, string) {
		t.Helper()
		var row, xmin string
		e := f.s.db.QueryRow(f.ctx, "SELECT row_to_json(r)::text,xmin::text FROM dsga_creation_reservations r WHERE operation_id=$1", op).Scan(&row, &xmin)
		if e != nil {
			t.Fatal(e)
		}
		return row, xmin
	}
	type original struct{ row, xmin string }
	before := map[string]original{}
	for _, op := range []string{"op_vm3_old_v1", "op_vm3_old_v2"} {
		row, xmin := read(op)
		before[op] = original{row, xmin}
	}
	forward3, e := migrations.ReadFile("migrations/900000067_dsga_reservation_vm_v3_placement.sql")
	if e != nil {
		t.Fatal(e)
	}
	f.exec(string(forward3))
	for op, want := range before {
		row, xmin := read(op)
		if row != want.row || xmin != want.xmin {
			t.Fatal("forward migration rewrote original payload/xmin")
		}
	}
	for _, version := range []uint32{1, 2} {
		candidate := draft
		candidate.Plan.VM = dsgaVMProfileFixture()
		candidate.Plan.VM.ProtocolVersion = version
		candidate.Plan.VM.Placement = nil
		if version == 1 {
			candidate.Plan.VM.ResourceLimits = nil
		}
		_, e = f.s.db.Exec(f.ctx, insert, keyHash("new-legacy-"+string(rune('0'+version))), "h1:"+strings.Repeat("b", 64), "op_vm3_new_legacy", marshal(candidate))
		if dsgaReservationPGCode(e) != "23514" {
			t.Fatal("new legacy reservation bypassed v3", e)
		}
	}
	// 保持历史v2首次绑定原凭据引用；不改draft，不能以恢复名义补新节点授权。
	ref, _ := json.Marshal(dsgaCredentialReference{1, "original-credential-uid", strings.Repeat("c", 64)})
	if _, e = f.s.db.Exec(f.ctx, "UPDATE dsga_creation_reservations SET credential_reference=$1::jsonb WHERE operation_id='op_vm3_old_v2'", ref); e != nil {
		t.Fatal("historical v2 source binding compatibility lost", e)
	}
	if _, e = f.s.db.Exec(f.ctx, "UPDATE dsga_creation_reservations SET draft=jsonb_set(draft,'{plan,vm,protocol_version}','3') WHERE operation_id='op_vm3_old_v2'"); dsgaReservationPGCode(e) != "23514" {
		t.Fatal("historical plan silently upgraded", e)
	}
	plan.VM = dsgaVMProfileFixture()
	draft.Plan = plan
	draft.ResourceName = "current"
	good := marshal(draft)
	add("new-v3-placement", "op_vm3_current", good)
	for name, mutate := range map[string]func(map[string]any){
		"missing_placement": func(v map[string]any) { delete(v, "placement") },
		"null_placement":    func(v map[string]any) { v["placement"] = nil },
		"bad_selector":      func(v map[string]any) { v["placement"].(map[string]any)["node_selector"] = []any{} },
		"empty_hosts":       func(v map[string]any) { v["placement"].(map[string]any)["allowed_node_names"] = []any{} },
		"duplicate_hosts": func(v map[string]any) {
			v["placement"].(map[string]any)["allowed_node_names"] = []any{"worker-117", "worker-117", "worker-119"}
		},
		"null_host": func(v map[string]any) {
			v["placement"].(map[string]any)["allowed_node_names"] = []any{"worker-117", nil, "worker-119"}
		},
		"missing_limits":  func(v map[string]any) { delete(v, "resource_limits") },
		"unknown_version": func(v map[string]any) { v["protocol_version"] = 4 },
	} {
		t.Run(name, func(t *testing.T) {
			var document map[string]any
			if json.Unmarshal(good, &document) != nil {
				t.Fatal("invalid fixture")
			}
			mutate(document["plan"].(map[string]any)["vm"].(map[string]any))
			bad, e := json.Marshal(document)
			if e != nil {
				t.Fatal(e)
			}
			_, e = f.s.db.Exec(f.ctx, insert, keyHash(name), "h1:"+strings.Repeat("d", 64), "op_vm3_bad", bad)
			if dsgaReservationPGCode(e) != "23514" {
				t.Fatal("partial v3 bypassed format guard", e)
			}
		})
	}
}
