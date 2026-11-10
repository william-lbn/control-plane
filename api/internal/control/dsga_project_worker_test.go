package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

// 公开接受→原Worker分派→真实元数据库失败提交/重试；Kube仍是TLS协议夹具。
// 故意缺少存储管理Service，不能把无执行器/未知来源误计为项目Ready。
func TestDSGAProjectWorkerDispatchFailureAndOriginalRetry(t *testing.T) {
	for _, version := range []uint32{17, 18} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			f := newDSGAProjectFixture(t)
			setDSGACatalog(t, dsgaReleaseCatalogFixture(t))
			f.body.PostgresVersion = int(version)
			f.exec(`UPDATE operations SET lease_expires_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, f.oldOperation)
			key := "worker-original-project"
			if w := f.request("creation_admin", key, f.body); w.Code != 202 {
				t.Fatal("project not accepted", w.Code)
			}
			op, before, xmin := f.reservation(key)
			for attempt := 1; attempt <= 2; attempt++ {
				if attempt == 2 {
					var project string
					if err := f.s.db.QueryRow(f.ctx, "SELECT project_id FROM operations WHERE id=$1", op).Scan(&project); err != nil {
						t.Fatal(err)
					}
					if err := f.s.retryFailed(f.ctx, op, project); err != nil {
						t.Fatal("original failed project cannot resume", err)
					}
				}
				if err := f.s.workOnce(f.ctx, "version-worker"); err != nil {
					t.Fatal("worker cannot commit result", err)
				}
				var state, first, project, branch, endpoint string
				var tries, changed int
				if err := f.s.db.QueryRow(f.ctx, `SELECT o.state,o.attempts,
 (SELECT state FROM operation_steps WHERE operation_id=o.id AND ordinal=0),
 p.state,b.state,e.state,
 (SELECT count(*) FROM operation_steps WHERE operation_id=o.id AND ordinal>0 AND state<>'queued')
 FROM operations o JOIN projects p ON p.id=o.project_id
 JOIN branches b ON b.id=o.payload->>'branch_id'
 JOIN endpoints e ON e.id=o.payload->>'endpoint_id' WHERE o.id=$1`, op).
					Scan(&state, &tries, &first, &project, &branch, &endpoint, &changed); err != nil {
					t.Fatal(err)
				}
				if state != "failed" || tries != attempt || first != "failed" || project != "error" || branch != "error" || endpoint != "error" || changed != 0 {
					t.Fatalf("unexpected original result: %s %d %s %s %s %s %d", state, tries, first, project, branch, endpoint, changed)
				}
			}
			afterOp, after, afterXmin := f.reservation(key)
			f.credentials.mu.Lock()
			posts := f.credentials.posts
			f.credentials.mu.Unlock()
			if op != afterOp || before != after || xmin != afterXmin || posts != 1 {
				t.Fatal("worker retry changed original plan or allocated new credentials")
			}
		})
	}
}

func TestDSGAProjectWorkerWaitUsesOriginalUIDAndTerminalState(t *testing.T) {
	ctx, binding, f, _, _, _, _, _ := initializationBindingFixture(t)
	if err := binding.waitOriginalCompute(ctx); err != nil {
		t.Fatal("same original Running VM rejected", err)
	}
	f.mu.Lock()
	vm := f.vm.DeepCopy()
	f.vm.Status.Phase = vmv1.VmFailed
	f.mu.Unlock()
	var wait vmWaitError
	if err := binding.waitOriginalCompute(ctx); !errors.As(err, &wait) || wait.phase != "Failed" {
		t.Fatal("terminal original VM was not rejected", err)
	}
	f.mu.Lock()
	f.vm = vm.DeepCopy()
	f.vm.UID = "replaced-original-vm"
	f.mu.Unlock()
	if err := binding.waitOriginalCompute(ctx); !errors.Is(err, errDSGACreationIdentity) {
		t.Fatal("replacement VM adopted", err)
	}
	f.mu.Lock()
	f.vm = vm.DeepCopy()
	f.vm.Status.Phase = ""
	f.mu.Unlock()
	short, cancel := context.WithTimeout(ctx, 75*time.Millisecond)
	defer cancel()
	if err := binding.waitOriginalCompute(short); !errors.As(err, &wait) {
		t.Fatal("pending startup did not remain bounded", err)
	}
	f.mu.Lock()
	f.vm = vm.DeepCopy()
	f.mu.Unlock()
	if _, err := binding.server.db.Exec(ctx, "UPDATE operations SET lease_owner='successor' WHERE id=$1", binding.operationID); err != nil {
		t.Fatal(err)
	}
	if err := binding.waitOriginalCompute(ctx); !errors.Is(err, errLeaseLost) {
		t.Fatal("stale worker continued readiness", err)
	}
}

func TestDSGAProjectWorkerInitializationPendingIsNotIdentitySuccess(t *testing.T) {
	ctx, binding, f, _, _, client, state, original := initializationBindingFixture(t)
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		state.mu.Lock()
		state.status = status
		state.mu.Unlock()
		fact, err := binding.observeInitializationWithHTTP(ctx, client)
		if err == nil || fact.ProtocolVersion != 0 || errors.Is(err, errDSGAInitializationPending) != (status == 503) {
			t.Fatalf("wrong pending classification: %d %v", status, err)
		}
	}
	state.mu.Lock()
	state.status = 0
	state.raw, _ = json.Marshal(original)
	state.mu.Unlock()
	if _, err := binding.observeInitializationWithHTTP(ctx, client); err != nil {
		t.Fatal("same source did not recover", err)
	}
	// 即使暂时503，只要VM UID变了也必须先拒绝，不能进入可等待分支。
	state.mu.Lock()
	state.status = 503
	state.mu.Unlock()
	f.mu.Lock()
	f.vm.UID = "replaced-during-initialization"
	f.mu.Unlock()
	before := state.calls.Load()
	if _, err := binding.observeInitializationWithHTTP(ctx, client); err == nil || errors.Is(err, errDSGAInitializationPending) || state.calls.Load() != before {
		t.Fatal("identity drift downgraded to startup pending")
	}
}

func TestDSGAProjectWorkerSQLResultRequiresActualVersionAndScope(t *testing.T) {
	for _, version := range []uint32{17, 18} {
		valid := sqlResult{Rows: [][]any{{int32(version*10000 + 2), false, dsgaApplicationDatabase, "control_probe"}}}
		if !validDSGAProjectSQL(valid, version) {
			t.Fatal("same version ordinary SQL rejected")
		}
		for name, row := range map[string][]any{
			"other_version": {int32((35-version)*10000 + 2), false, dsgaApplicationDatabase, "control_probe"},
			"replica":       {int32(version * 10000), true, dsgaApplicationDatabase, "control_probe"},
			"wrong_db":      {int32(version * 10000), false, "postgres", "control_probe"},
			"admin_role":    {int32(version * 10000), false, dsgaApplicationDatabase, "cloud_admin"},
			"string_value":  {fmt.Sprint(version * 10000), false, dsgaApplicationDatabase, "control_probe"},
			"missing_field": {int32(version * 10000), false},
		} {
			t.Run(fmt.Sprint(version)+"/"+name, func(t *testing.T) {
				if validDSGAProjectSQL(sqlResult{Rows: [][]any{row}}, version) {
					t.Fatal("wrong version or scope accepted")
				}
			})
		}
		valid.Truncated = true
		if validDSGAProjectSQL(valid, version) {
			t.Fatal("truncated response accepted")
		}
	}
}

// Ready提交只验证元数据事务，不冒充真实SQL或物理资格：前七步成功在这里是明确
// 测试前置，初始化来自已有原生HTTP协议夹具，K8s产品验收仍必须另跑。
func TestDSGAProjectWorkerReadyRequiresCompleteEvidenceAndKeepsHistory(t *testing.T) {
	ctx, binding, _, _, _, client, _, _ := initializationBindingFixture(t, "create_project")
	fact, err := binding.observeInitializationWithHTTP(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if err := binding.recordInitialization(ctx, fact); err != nil {
		t.Fatal(err)
	}
	if err := binding.persistProjectReady(ctx, fact); err == nil {
		t.Fatal("closed admission or incomplete steps marked Ready")
	}
	fact.Observed.AdmissionOpen = true
	fact.Observed.AdmissionRevision = 1
	fact.Observed.IssuerFloorEpoch = 1
	if err := binding.persistProjectReady(ctx, fact); err == nil {
		t.Fatal("missing successful SQL step marked Ready")
	}
	if _, err := binding.server.db.Exec(ctx, "UPDATE operation_steps SET state='succeeded' WHERE operation_id=$1 AND ordinal<7", binding.operationID); err != nil {
		t.Fatal(err)
	}
	wrongBoot := fact
	wrongBoot.Observed.Instance.PGBootID++
	if err := binding.persistProjectReady(ctx, wrongBoot); err == nil {
		t.Fatal("unrecorded boot marked Ready")
	}
	read := func() (string, string) {
		t.Helper()
		var state, version string
		if err := binding.server.db.QueryRow(ctx, `SELECT p.state||'/'||b.state||'/'||e.state||'/'||s.observed_state,
 p.version::text||'/'||b.version::text||'/'||e.version::text||'/'||s.version::text
 FROM projects p JOIN branches b ON b.project_id=p.id JOIN endpoints e ON e.branch_id=b.id
 JOIN branch_service_instances s ON s.branch_id=b.id AND s.service_kind='postgres'
 WHERE e.id=$1`, binding.intent.EndpointID).Scan(&state, &version); err != nil {
			t.Fatal(err)
		}
		return state, version
	}
	if err := binding.persistProjectReady(ctx, fact); err != nil {
		t.Fatal("complete current evidence rejected", err)
	}
	state, versions := read()
	if state != "ready/ready/active/active" {
		t.Fatal("partial readiness transaction", state)
	}
	if err := binding.persistProjectReady(ctx, fact); err != nil {
		t.Fatal("same original confirmation not replayable", err)
	}
	if again, replayVersions := read(); again != state || replayVersions != versions {
		t.Fatal("repeat confirmation changed resource generation")
	}
	var firstAdmission bool
	if err := binding.server.db.QueryRow(ctx, `SELECT (receipt#>>'{observed,admission_open}')::boolean FROM dsga_creation_initialization_observations WHERE operation_id=$1`, binding.operationID).Scan(&firstAdmission); err != nil || firstAdmission {
		t.Fatal("current confirmation rewrote first initialization observation", err)
	}
	if _, err := binding.server.db.Exec(ctx, "UPDATE operations SET lease_owner='successor' WHERE id=$1", binding.operationID); err != nil {
		t.Fatal(err)
	}
	if err := binding.persistProjectReady(ctx, fact); !errors.Is(err, errLeaseLost) {
		t.Fatal("stale worker committed readiness", err)
	}
}

func TestDSGAProjectWorkerLeaseCannotReviveAfterWaitingForLock(t *testing.T) {
	for _, renew := range []bool{false, true} {
		t.Run(fmt.Sprintf("renew=%t", renew), func(t *testing.T) {
			ctx, s, op, _, _ := dsgaBindingFixture(t)
			if _, err := s.db.Exec(ctx, "UPDATE operations SET lease_expires_at=clock_timestamp()+interval '250 milliseconds' WHERE id=$1", op); err != nil {
				t.Fatal(err)
			}
			holder, err := s.db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Rollback(ctx)
			if _, err := holder.Exec(ctx, "SELECT id FROM operations WHERE id=$1 FOR UPDATE", op); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				if renew {
					done <- s.renewOperationLease(ctx, op, "worker_a")
					return
				}
				tx, err := s.db.Begin(ctx)
				if err == nil {
					err = assertCreateLeaseTx(ctx, tx, op, "worker_a")
					_ = tx.Rollback(ctx)
				}
				done <- err
			}()
			// 用元数据库实际时钟确认租约过期，同时保证被测调用尚未越过行锁。
			for {
				var expired bool
				if err := s.db.QueryRow(ctx, "SELECT lease_expires_at<clock_timestamp() FROM operations WHERE id=$1", op).Scan(&expired); err != nil {
					t.Fatal(err)
				}
				if expired {
					break
				}
				select {
				case err := <-done:
					t.Fatal("lease check bypassed original row lock", err)
				case <-time.After(10 * time.Millisecond):
				}
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, errLeaseLost) {
					t.Fatal("expired lease revived after waiting", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("lease check did not finish")
			}
		})
	}
}
