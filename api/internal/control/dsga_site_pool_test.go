package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// 使用真实迁移和公开创建事务验证节点池；不能只测试Go校验而漏掉数据库守卫。
func TestDSGASitePoolDatabaseGuard(t *testing.T) {
	for _, count := range []int{1, 2, 5, 128} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f := newDSGAProjectFixture(t)
			_, policy, _ := dsgaPlanFixture(t)
			policy.VM = dsgaVMProfileV5Fixture()
			policy.VM.Placement.NodeSelector = map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64"}
			policy.VM.Placement.AllowedNodeNames = make([]string, count)
			for i := range policy.VM.Placement.AllowedNodeNames {
				policy.VM.Placement.AllowedNodeNames[i] = fmt.Sprintf("site-worker-%d", i+1)
			}
			raw, err := json.Marshal(dsgaProjectPolicy{ProtocolVersion: 1, Region: "test-region", Bounds: createBounds{500, 2000, 1024, 4096}, Creation: policy})
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("NEON_DSGA_CREATION_POLICY", string(raw))
			key := "site-pool-original-request"
			if w := f.request("creation_admin", key, f.body); w.Code != 202 {
				t.Fatalf("valid %d-node pool rejected by public creation transaction: %d %s", count, w.Code, w.Body.String())
			}
			_, draft, _ := f.reservation(key)
			if _, err := f.s.db.Exec(f.ctx, `UPDATE dsga_creation_reservations SET draft=jsonb_set(draft,'{plan,vm,placement,allowed_node_names}','["replacement"]'::jsonb) WHERE actor_id='creation_admin' AND key_hash=$1`, keyHash(key)); !sitePoolCheckViolation(err) {
				t.Fatalf("frozen original pool could be changed: %v", err)
			}
			if count != 5 {
				return
			}
			var original map[string]any
			if err := json.Unmarshal([]byte(draft), &original); err != nil {
				t.Fatal(err)
			}
			placement := original["plan"].(map[string]any)["vm"].(map[string]any)["placement"].(map[string]any)
			tooMany := make([]string, 129)
			for i := range tooMany {
				tooMany[i] = fmt.Sprintf("site-worker-%d", i+1)
			}
			for _, bad := range []any{[]string{}, []string{"duplicate", "duplicate"}, tooMany, []any{"valid", 17}, []string{""}} {
				placement["allowed_node_names"] = bad
				candidate, err := json.Marshal(original)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.s.db.Exec(f.ctx, `INSERT INTO dsga_creation_reservations(actor_id,key_hash,request_hash,scope,org_id,base_action,operation_id,draft) VALUES('creation_admin',$1,$2,'POST:/organizations/org_creation/projects','org_creation','create_project',$3,$4)`, keyHash(newID("key_")), "h1:"+strings.Repeat("a", 64), newID("op_"), candidate)
				if !sitePoolCheckViolation(err) {
					t.Fatalf("invalid pool passed database guard or failed for unrelated reason: %v", err)
				}
			}
		})
	}
}

func sitePoolCheckViolation(err error) bool {
	var pgerr *pgconn.PgError
	return errors.As(err, &pgerr) && pgerr.Code == "23514"
}
