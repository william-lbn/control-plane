package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/budget"
	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// 原AS initial-domain完整unit领域夹具；绝非真实平台资格/生产凭据。
// WAL始终经原Store typed方法/CAS/codec生成，不手写压缩注解或放宽解析器。
func normalGatewayPrepare(t *testing.T, f *brokerTestAPI) (*journal.Store, *journal.Snapshot, budget.PrepareCommand) {
	t.Helper()
	raw, err := os.ReadFile("testdata/dsga-normal-initial-domain.json")
	if err != nil {
		t.Fatal(err)
	}
	var domain struct {
		Initial budget.Initial                 `json:"initial"`
		Command budget.InitialAdmissionCommand `json:"command"`
		Receipt budget.InitialAdmissionReceipt `json:"receipt"`
	}
	if err = json.Unmarshal(raw, &domain); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	vm := f.vm.DeepCopy()
	f.mu.Unlock()
	binding := journal.Binding{Namespace: vm.Namespace, Name: vm.Name, VMUID: vm.UID, Incarnation: vm.Spec.DSGABootstrap.Incarnation}
	store, err := journal.NewStore(f.proxy.broker.vms.NeonvmV1().VirtualMachines(vm.Namespace), binding)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snapshot, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	actor := snapshot.State().Owner
	i, first, opened := domain.Initial, domain.Command, domain.Receipt
	// 只把unit incarnation对齐现有broker typed API；完整初开摘要由原Digest重算。
	i.Incarnation = binding.Incarnation
	first.InitialSHA256, err = i.Digest()
	if err != nil {
		t.Fatal(err)
	}
	opened.Command = first
	if err = opened.ValidateFor(i, first, actor.Epoch); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.BindResource(ctx, snapshot, actor, i.PGAggregate.Identity)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.BindPG(ctx, snapshot, actor, i.Instance)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.BindInitialBudget(ctx, snapshot, actor, i)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _, err = store.StartInitialAdmission(ctx, snapshot, actor, first)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.RecordInitialAdmission(ctx, snapshot, actor, opened)
	if err != nil {
		t.Fatal(err)
	}
	request := dsgapi.Request{ProtocolVersion: 1, Key: dsgapi.Key{Instance: i.Instance, IssuerEpoch: actor.Epoch, Revision: 1, OperationID: dsgapi.OperationID{High: 123, Low: 1}}, Kind: dsgapi.Grow, ExpectedCapacityGeneration: opened.Shared.Observed.CapacityGeneration, TargetBuffers: 1024, TargetVMMemoryBytes: 2 << 30, BudgetCommitmentID: i.PGAggregate.Bootstrap.BudgetCommitmentID, DeadlineUnixMillis: 1000}
	var disposition journal.Disposition
	snapshot, disposition, err = store.Begin(ctx, snapshot, actor, request, 0)
	if err != nil || disposition != journal.Accepted {
		t.Fatalf("original Begin: %v / %s", err, disposition)
	}
	upper, err := budget.GrowSharedUpper(opened.Shared, request.TargetBuffers)
	if err != nil {
		t.Fatal(err)
	}
	prepared := budget.Prepared{ProtocolVersion: 1, Request: request, Identity: i.PGAggregate.Identity, RunnerPodName: i.RunnerPodName, RunnerRestartCount: i.RunnerRestartCount, VMBeforeMemoryBytes: i.VMMemoryBytes, GuestUsableMemoryBytes: (2 << 30) - (256 << 20), Shared: opened.Shared, TargetSharedUpperBytes: upper, Limits: i.PGAggregate.Bootstrap.Limits, SystemOutsidePG: i.SystemOutsidePG, Transition: i.Transition, Safety: i.Safety, NormalQuotaProtocolVersion: 1, NormalQuotaFence: &dsgapi.FenceResponse{ProtocolVersion: 1, RequestedInstance: request.Key.Instance, RequestedEpoch: request.Key.IssuerEpoch, Disposition: dsgapi.Fenced, Observed: opened.Shared.Observed}}
	qualification := budget.QualificationSource{Claim: budget.ObjectRef{Name: "qualification", UID: "qualification-uid"}, SHA256: strings.Repeat("ab", 32)}
	prefix := fmt.Sprintf("qualified-%s-%d-", qualification.SHA256, request.TargetVMMemoryBytes)
	prepared.SystemOutsidePG.SourceID, prepared.Transition.SourceID, prepared.Safety.SourceID = prefix+"system", prefix+"transition", prefix+"safety"
	command := budget.PrepareCommand{ProtocolVersion: 1, Qualification: qualification, Prepared: prepared, RootSHA256: strings.Repeat("cd", 32)}
	command.OutsideName, err = command.CanonicalOutsideName()
	if err != nil {
		t.Fatal(err)
	}
	if err = command.Validate(); err != nil {
		t.Fatal(err)
	}
	return store, snapshot, command
}
func normalGatewayBytes(t *testing.T, command budget.PrepareCommand, size int) []byte {
	t.Helper()
	raw, err := json.Marshal(command)
	if err != nil || len(raw) > size {
		t.Fatalf("bounded unit command %d: %v", len(raw), err)
	}
	return append(raw, bytes.Repeat([]byte(" "), size-len(raw))...)
}
func startNormalGatewayWAL(t *testing.T, f *brokerTestAPI, size int) {
	t.Helper()
	store, snapshot, command := normalGatewayPrepare(t, f)
	raw := normalGatewayBytes(t, command, size)
	after, _, err := store.Start(context.Background(), snapshot, snapshot.State().Owner, command.Prepared.Request.Key, journal.PrepareBudget, command.Prepared.Request.Key.Instance.ComputeInstanceID, raw)
	if err != nil {
		t.Fatal(err)
	}
	readback, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	step := readback.State().Active.Steps[0]
	if !bytes.Equal(raw, step.Command) || step.CommandSHA256 != after.State().Active.Steps[0].CommandSHA256 {
		t.Fatal("original full command/digest lost through codec")
	}
}

// 真实生产TLS mux/短scoped令牌/Owner/Store读者；Kube和native仅是明确unit协议peer。
func TestDSGANormalLargeJournalReadbackAndScopedProxy(t *testing.T) {
	for _, size := range []int{8193, 16384} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			var calls atomic.Int32
			f, client, base, token := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"unit_normal_read":true}`))
			})
			startNormalGatewayWAL(t, f, size)
			status, body := proxyRequest(t, client, http.MethodGet, base+"/dsg/v1/resource/state", token, nil)
			if status != 200 || string(body) != `{"unit_normal_read":true}` || calls.Load() != 1 {
				t.Fatalf("valid large normal WAL blocked scoped read: %d calls=%d", status, calls.Load())
			}
		})
	}
}
func TestDSGANormalLargeJournalKeepsScopedOwnerLeaseGates(t *testing.T) {
	for _, mode := range []string{"owner", "lease_expired", "general_token"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			f, client, base, token := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			})
			startNormalGatewayWAL(t, f, 8193)
			f.mu.Lock()
			switch mode {
			case "owner":
				holder := "dsga-cccccccccccccccccccccccccccccccc.replacement"
				f.lease.Spec.HolderIdentity = &holder
			case "lease_expired":
				past := metav1.NewMicroTime(time.Now().Add(-time.Hour))
				f.lease.Spec.RenewTime = &past
			case "general_token":
				key := ed25519.NewKeyFromSeed(f.credentials.Data["controlSigningSeed"])
				token = computeJWT(key, "ep_0123456789abcdef", time.Now())
			}
			f.mu.Unlock()
			status, _ := proxyRequest(t, client, http.MethodGet, base+"/dsg/v1/resource/state", token, nil)
			if status < 400 || calls.Load() != 0 {
				t.Fatalf("large WAL bypassed original %s gate: %d", mode, status)
			}
		})
	}
}
func TestDSGANormalLargeJournalRejectsIncompletePrepare(t *testing.T) {
	for _, mode := range []string{"missing_fence", "legacy_prepare", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			f, _, _, _ := proxyFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("rejected WAL reached native") })
			store, snapshot, command := normalGatewayPrepare(t, f)
			size := 8193
			switch mode {
			case "missing_fence":
				command.Prepared.NormalQuotaFence = nil
			case "legacy_prepare":
				command.Prepared.NormalQuotaProtocolVersion = 0
				command.Prepared.NormalQuotaFence = nil
			case "oversize":
				size = 16385
			}
			// 先重算原canonical名，避免负例仅因陈旧OutsideName被拒而掩盖fence/legacy门。
			var err error
			command.OutsideName, err = command.CanonicalOutsideName()
			if err != nil {
				t.Fatal(err)
			}
			raw := normalGatewayBytes(t, command, size)
			_, _, err = store.Start(context.Background(), snapshot, snapshot.State().Owner, command.Prepared.Request.Key, journal.PrepareBudget, command.Prepared.Request.Key.Instance.ComputeInstanceID, raw)
			if err == nil {
				t.Fatal("incomplete or oversized normal Prepare accepted")
			}
		})
	}
}
