package control

import (
	"context"
	"encoding/json"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/budget"
	"github.com/neondatabase/autoscaling/pkg/dsga/runtimeselection"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"reflect"
	"strings"
	"testing"
	"time"
)

func selectionFixture(t *testing.T) runtimeselection.Selection {
	p, plan := supplyFixture(t, 17)
	return runtimeselection.Selection{ProtocolVersion: 1, Namespace: plan.Namespace, Name: kubeName(p.EndpointID), Incarnation: plan.Incarnation, Instance: dsgapi.Instance{VMUID: "original-vm", ComputeInstanceID: plan.ComputeInstanceID, PGBootID: 17}, Generation: 1,
		Qualification: budget.QualificationSource{Claim: budget.ObjectRef{Name: "qualification", UID: "qualification-uid"}, SHA256: strings.Repeat("a", 64)}, Automatic: &plan.VM.supply().clone().Automatic}
}
func TestDSGAMemoryPolicyRangeUsesOriginalClassAndCommonVersion(t *testing.T) {
	for _, v := range []uint32{17, 18} {
		p, plan := supplyFixture(t, v)
		e := &dsgaEndpointRuntime{Payload: p, Plan: plan}
		b := boundsPayload{p.ProjectID, p.EndpointID, kubeName(p.EndpointID), p.MinCPU, p.MaxCPU, p.MaxMem, p.MaxMem}
		if e.validateBounds(b) != nil {
			t.Fatal("fixed upper target should use original grow path")
		}
		b.MinMem = p.MinMem
		b.MaxMem = p.MinMem
		if e.validateBounds(b) != nil {
			t.Fatal("fixed lower target should use original safe shrink path")
		}
		for _, kind := range []string{"capacity", "cpu", "identity", "offslot", "noexact"} {
			q := b
			switch kind {
			case "capacity":
				q.MaxMem = p.MaxMem + 1024
			case "cpu":
				q.MinCPU += 250
			case "identity":
				q.EndpointID = "foreign"
			case "offslot":
				q.MinMem++
			case "noexact":
				q.MinMem = (p.MinMem + p.MaxMem) / 2
				q.MaxMem = q.MinMem
			}
			if e.validateBounds(q) == nil {
				t.Fatal("unsupported target accepted", kind)
			}
		}
	}
}
func TestDSGAMemoryPolicyReadbackRejectsStaleForeignAndOldGeneration(t *testing.T) {
	selected := selectionFixture(t)
	now := time.Now().UTC()
	status := runtimeselection.Status{DesiredGeneration: 1, AppliedGeneration: 1, DesiredSHA256: selected.Digest(), AppliedSHA256: selected.Digest(), State: "applied"}
	for _, mode := range []string{"current", "pending", "old-generation", "wrong-digest", "old-boot", "old-vm", "stopped", "missing-stop", "stale", "future", "wrong-pod"} {
		t.Run(mode, func(t *testing.T) {
			instance := selected.Instance
			state := status
			observed := now
			pod := "original-pod"
			dump := map[string]any{"stopped": false}
			switch mode {
			case "pending":
				state.AppliedGeneration = 0
				state.AppliedSHA256 = ""
				state.State = "waiting_for_original_operation"
			case "old-generation":
				state.DesiredGeneration = 2
			case "wrong-digest":
				state.DesiredSHA256 = strings.Repeat("f", 64)
			case "old-boot":
				instance.PGBootID++
			case "old-vm":
				instance.VMUID = "different-vm"
			case "stopped":
				dump["stopped"] = true
			case "missing-stop":
				delete(dump, "stopped")
			case "stale":
				observed = now.Add(-3 * time.Minute)
			case "future":
				observed = now.Add(time.Minute)
			case "wrong-pod":
				pod = "different-pod"
			}
			dump["pods"] = []any{map[string]any{"podName": map[string]string{"namespace": selected.Namespace, "name": pod}, "runner": map[string]any{"dsga": map[string]any{"instance": instance, "observedAt": observed, "runtimeSelection": state}}}}
			raw, _ := json.Marshal(dump)
			got, err := decodeDSGAOwnerPolicy(raw, selected.Namespace, "original-pod", selected, now)
			if mode == "current" || mode == "pending" {
				if err != nil || !reflect.DeepEqual(got, state) {
					t.Fatal("valid state rejected", err)
				}
			} else if err == nil {
				t.Fatal("unconfirmed state exposed as current")
			}
		})
	}
}
func TestDSGAMemoryPolicyOriginalIntentNeverAdvancesOnReplay(t *testing.T) {
	ctx, b, _, _, _, _, _, _ := initializationBindingFixture(t)
	p, plan := supplyFixture(t, 17)
	// 使用原创建fixture的项目/端点，只测试原端点操作锁和代次持久化。
	p.ProjectID = b.intent.ProjectID
	p.EndpointID = b.intent.EndpointID
	e := &dsgaEndpointRuntime{Payload: p, Plan: plan}
	bounds := boundsPayload{p.ProjectID, p.EndpointID, kubeName(p.EndpointID), p.MinCPU, p.MaxCPU, p.MinMem, p.MaxMem}
	raw, _ := json.Marshal(bounds)
	id := "policy-test-operation"
	// 元数据单元场景：原创建操作先结束，保持生产的一端点单活动操作约束。
	if _, err := b.server.db.Exec(ctx, "UPDATE operations SET state='succeeded',lease_owner=NULL,lease_expires_at=NULL WHERE id=$1", b.operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.server.db.Exec(ctx, "UPDATE endpoints SET state='active' WHERE id=$1", p.EndpointID); err != nil {
		t.Fatal(err)
	}
	_, err := b.server.db.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,lease_owner,lease_expires_at)
 VALUES($1,$2,'endpoint',$3,'update_endpoint','running','local-admin','policy-test',$4,$5,clock_timestamp()+interval '5 minutes')`, id, p.ProjectID, p.EndpointID, raw, b.workerID)
	if err != nil {
		t.Fatal(err)
	}
	current := selectionFixture(t)
	cm := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{UID: "selection-original"}}
	first, err := b.server.dsgaPolicyIntent(ctx, id, b.workerID, e, bounds, cm, current)
	if err != nil {
		t.Fatal(err)
	}
	if first.Next.Generation != 2 {
		t.Fatal("wrong first generation")
	}
	// 外部PUT已成功而回复丢失：同Operation仍返回原2，绝不递增成3。
	second, err := b.server.dsgaPolicyIntent(ctx, id, b.workerID, e, bounds, cm, first.Next)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("unknown write replay changed intent", err)
	}
	if _, err = b.server.db.Exec(ctx, "UPDATE dsga_runtime_policy_intents SET intent='{}'"); err == nil {
		t.Fatal("immutable policy intent overwritten")
	}
	if _, err = b.server.db.Exec(ctx, "UPDATE operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	if _, err = b.server.dsgaPolicyIntent(context.Background(), id, b.workerID, e, bounds, cm, first.Next); err == nil {
		t.Fatal("expired writer accepted")
	}
}
