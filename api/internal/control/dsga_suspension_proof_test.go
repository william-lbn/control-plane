package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/hostguard"
	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	"github.com/neondatabase/autoscaling/pkg/dsga/runtimeselection"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestDSGASuspensionHostRequiresOriginalNodeAndRetiredPlacement(t *testing.T) {
	binding := hostguard.Binding{NodeName: "worker-fixture", NodeUID: "node-1", Namespace: "platform",
		LeaseName: "host-lease", LeaseUID: "lease-1"}
	base := hostguard.Model{ProtocolVersion: 5, Binding: binding, Origin: strings.Repeat("b", 32),
		LeaseFloor: journal.Owner{Epoch: 1, HolderID: "owner-1"}, Sequence: 10, OutsideBytes: 1 << 30, Holds: []hostguard.Hold{},
		Profile: &hostguard.Profile{ID: "fixture", SchedulerName: "autoscale-scheduler", VMOverheadBytes: 64 << 20, PodOverheadBytes: 32 << 20}}
	node := func(sequence dsgapi.Uint64, placements []hostguard.Placement) *core.Node {
		t.Helper()
		m := base
		m.Sequence, m.Placements = sequence, &placements
		if err := m.Validate(); err != nil {
			t.Fatal(err)
		}
		wire, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return &core.Node{ObjectMeta: metav1.ObjectMeta{Name: binding.NodeName, UID: binding.NodeUID, ResourceVersion: "1",
			Annotations: map[string]string{hostguard.LedgerAnnotation: string(wire), hostguard.OriginAnnotation: "1:" + base.Origin},
			Finalizers:  []string{hostguard.ProtectionFinalizer}}}
	}
	placement := hostguard.Placement{Namespace: "neon", PodName: "runner", PodUID: "runner-1",
		VMName: "compute", VMUID: "vm-1", Incarnation: strings.Repeat("a", 32), Managed: true,
		MemoryBytes: 4 << 30, HostOverheadBytes: 64 << 20}
	first := node(10, []hostguard.Placement{placement})
	original, _, err := readDSGASuspendedHost(first)
	if err != nil {
		t.Fatal(err)
	}
	stillHeld := node(11, []hostguard.Placement{placement})
	released := node(11, []hostguard.Placement{})
	if err := verifyDSGASuspensionHostSettlement(original, released, "vm-1", "runner-1"); err != nil {
		t.Fatal(err)
	}
	wrongNode := released.DeepCopy()
	wrongNode.UID = "node-2"
	badOrigin := released.DeepCopy()
	badOrigin.Annotations[hostguard.OriginAnnotation] = "1:" + strings.Repeat("c", 32)
	for _, rejected := range []*core.Node{first, stillHeld, wrongNode, badOrigin} {
		if err := verifyDSGASuspensionHostSettlement(original, rejected, "vm-1", "runner-1"); err == nil {
			t.Fatal("unconfirmed Host settlement was accepted")
		}
	}
}

func TestDSGASuspensionIntentPinsBootAndHostAcrossWorkerRetry(t *testing.T) {
	ctx, b, fact := dsgaSuspensionAdmissionFixture(t)
	p := dsgaSuspensionPayload{ProjectID: b.intent.ProjectID, BranchID: b.intent.BranchID, EndpointID: b.intent.EndpointID,
		CreationOperation: b.operationID, VMUID: fact.Observed.Instance.VMUID}
	binding := journal.Binding{Namespace: b.intent.Creation.Namespace, Name: kubeName(p.EndpointID),
		VMUID: types.UID(p.VMUID), Incarnation: b.intent.Creation.Incarnation}
	id := "suspension-intent-test"
	payload, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.server.db.Exec(ctx, `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,lease_owner,lease_expires_at)
 VALUES($1,$2,'endpoint',$3,$4,'queued','local-admin','suspension-test',$5,NULL,NULL)`,
		id, p.ProjectID, p.EndpointID, dsgaSuspendAction, payload)
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.server.db.Exec(ctx, `UPDATE operations SET state='running',lease_owner=$2,lease_expires_at=clock_timestamp()+interval '5 minutes' WHERE id=$1`, id, b.workerID)
	if err != nil {
		t.Fatal(err)
	}
	selected := selectionFixture(t)
	selected.Namespace, selected.Name, selected.Incarnation = binding.Namespace, binding.Name, binding.Incarnation
	selected.Instance.VMUID, selected.Instance.ComputeInstanceID = p.VMUID, b.intent.Creation.ComputeInstanceID
	vm := &vmv1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Namespace: binding.Namespace, Name: binding.Name, UID: binding.VMUID}}
	vm.Status.Node, vm.Status.PodName = "worker-test", "runner-original"
	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: binding.Namespace, Name: vm.Status.PodName, UID: "runner-uid",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(vm, vmv1.SchemeGroupVersion.WithKind("VirtualMachine"))}},
		Spec: core.PodSpec{NodeName: vm.Status.Node, SchedulerName: "autoscale-scheduler"}, Status: core.PodStatus{Phase: core.PodRunning}}
	placements := []hostguard.Placement{{Namespace: vm.Namespace, PodName: pod.Name, PodUID: string(pod.UID), VMName: vm.Name,
		VMUID: string(vm.UID), Incarnation: binding.Incarnation, Managed: true, MemoryBytes: 4 << 30, HostOverheadBytes: 64 << 20}}
	host := hostguard.Model{ProtocolVersion: 5, Binding: hostguard.Binding{NodeName: vm.Status.Node, NodeUID: "node-uid",
		Namespace: "platform", LeaseName: "host-lease", LeaseUID: "lease-uid"}, Origin: strings.Repeat("b", 32),
		LeaseFloor: journal.Owner{Epoch: 1, HolderID: "owner-1"}, Sequence: 10, OutsideBytes: 1 << 30,
		Holds: []hostguard.Hold{}, Placements: &placements,
		Profile: &hostguard.Profile{ID: "fixture", SchedulerName: pod.Spec.SchedulerName, VMOverheadBytes: 64 << 20, PodOverheadBytes: 32 << 20}}
	wire, err := json.Marshal(host)
	if err != nil {
		t.Fatal(err)
	}
	node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: host.Binding.NodeName, UID: host.Binding.NodeUID, ResourceVersion: "1",
		Annotations: map[string]string{hostguard.LedgerAnnotation: string(wire), hostguard.OriginAnnotation: "1:" + host.Origin},
		Finalizers:  []string{hostguard.ProtectionFinalizer}}}
	cm := &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{UID: "selection-original"}}
	proposed, err := newDSGASuspensionIntent(id, vm, cm, selected, pod, node, binding)
	if err != nil || proposed.Selection.Next.Generation != selected.Generation+1 || proposed.Selection.Next.Automatic != nil ||
		proposed.Selection.Next.SuspensionOperation != id || proposed.Selection.PreviousSHA256 != selected.Digest() {
		t.Fatalf("original selection was not frozen: %v", err)
	}
	foreign := pod.DeepCopy()
	foreign.UID = "replacement-runner"
	if _, err := newDSGASuspensionIntent(id, vm, cm, selected, foreign, node, binding); err == nil {
		t.Fatal("Runner without the original Host charge was accepted")
	}
	first, err := b.server.dsgaSuspensionIntent(ctx, id, b.workerID, p, binding, &proposed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.server.db.Exec(ctx, "UPDATE dsga_suspension_intents SET intent='{}' WHERE operation_id=$1", id); err == nil {
		t.Fatal("durable suspension intent could be changed")
	}
	if _, err := b.server.db.Exec(ctx, "UPDATE operations SET lease_owner='replacement-worker' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	proposed.RunnerUID = "replacement-runner"
	proposed.Selection.Next.Instance.PGBootID++
	replay, err := b.server.dsgaSuspensionIntent(ctx, id, "replacement-worker", p, binding, &proposed)
	if err != nil || !reflect.DeepEqual(first, replay) {
		t.Fatalf("takeover changed the original frozen intent: %v", err)
	}
	if _, err := b.server.dsgaSuspensionIntent(context.Background(), id, b.workerID, p, binding, nil); !errors.Is(err, errLeaseLost) {
		t.Fatalf("old Worker accepted: %v", err)
	}
	vm.ResourceVersion = "42"
	if err := b.server.recordDSGASuspensionRetirement(ctx, id, b.workerID, vm); !errors.Is(err, errLeaseLost) {
		t.Fatalf("old Worker could authorize retirement: %v", err)
	}
	if err := b.server.recordDSGASuspensionRetirement(ctx, id, "replacement-worker", vm); err != nil {
		t.Fatal(err)
	}
	if _, err := b.server.db.Exec(ctx, "DELETE FROM dsga_suspension_retirements WHERE operation_id=$1", id); err == nil {
		t.Fatal("retirement authorization could be removed")
	}
	if _, err := b.server.db.Exec(ctx, `UPDATE operations SET state='succeeded',finished_at=clock_timestamp(),lease_owner=NULL,lease_expires_at=NULL
 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	tx, err := b.server.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := commitDSGASuspension(ctx, tx, id, p.ProjectID, p.EndpointID); !errors.Is(err, errDSGACreationIdentity) {
		t.Fatalf("endpoint suspended before step receipts: %v", err)
	}
	for i, name := range dsgaSuspensionSteps {
		if _, err := tx.Exec(ctx, `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,$2,$3,'succeeded')`,
			id, i, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := commitDSGASuspension(ctx, tx, id, p.ProjectID, p.EndpointID); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := tx.QueryRow(ctx, "SELECT state FROM endpoints WHERE id=$1", p.EndpointID).Scan(&state); err != nil || state != "suspended" {
		t.Fatalf("operation did not finalize the original endpoint: %v", err)
	}
	original, err := readDSGAResumeSource(ctx, tx, b.intent.createPayload, id)
	if err != nil || original.Creation != b.operationID || original.VMUID != p.VMUID || original.PGBootID != selected.Instance.PGBootID ||
		original.ServiceUID == "" || original.ConfigUID == "" {
		t.Fatalf("resume lacks the authoritative retired generation: %v", err)
	}
	foreignTimeline := b.intent.createPayload
	foreignTimeline.TimelineID = strings.Repeat("e", 32)
	if _, err = readDSGAResumeSource(ctx, tx, foreignTimeline, id); err == nil {
		t.Fatal("resume accepted a different Timeline")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	t.Run("public-resume-and-next-generation", func(t *testing.T) {
		testDSGAResumeGeneration(t, ctx, b, fact, original)
	})
}

func testDSGAResumeGeneration(t *testing.T, ctx context.Context, b *dsgaCreationBinding, fact dsgaInitializationResponse, source dsgaResumeSource) {
	t.Helper()
	s := b.server
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	p := b.intent.createPayload
	previous, err := readDSGAResumePlan(ctx, s.db, source, p)
	if err != nil {
		t.Fatal(err)
	}
	_, release := supplyFixture(t, previous.PostgresVersion)
	setDSGACatalog(t, dsgaRuntimeCatalog{1, []dsgaProjectPolicy{{1, "test", createBounds{p.MinCPU, p.MaxCPU, p.MinMem, p.MaxMem}, dsgaCreationPolicy{
		PostgresVersion: previous.PostgresVersion, NativeBootstrapVersion: release.NativeBootstrapVersion,
		Namespace: previous.Namespace, Image: release.Image, MemorySlotBytes: release.MemorySlotBytes, Budget: release.Budget,
		VM: release.VM, Storage: previous.Storage, RetainedCatalog: true,
	}}}})
	s.idempotencyKey = []byte(strings.Repeat("k", 32))
	t.Setenv("NEON_V2_CREATE_ENABLED", "true")
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('resume-admin','resume-admin','unused','viewer')`)
	exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('local','resume-admin','admin')`)
	exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,'resume-admin',$2,clock_timestamp()+interval '1 hour')`, digest("resume-session"), digest("resume-csrf"))
	exec(`UPDATE endpoints SET min_cpu_milli=$2,max_cpu_milli=$3,min_memory_mib=$4,max_memory_mib=$5 WHERE id=$1`, p.EndpointID, p.MinCPU, p.MaxCPU, p.MinMem, p.MaxMem)
	router := s.routes()
	post := func(action, version, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+p.ProjectID+"/endpoints/"+p.EndpointID+"/"+action, nil)
		req.AddCookie(&http.Cookie{Name: "neon_v2_session", Value: "resume-session"})
		req.AddCookie(&http.Cookie{Name: "neon_v2_csrf", Value: "resume-csrf"})
		req.Header.Set("X-CSRF-Token", "resume-csrf")
		req.Header.Set("If-Match", version)
		req.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	t.Setenv("NEON_DSGA_SUSPENSION_ENABLED", "false")
	if w := post("resume", `"2"`, "same-resume-intent"); w.Code != 409 {
		t.Fatalf("disabled driver accepted resume: %d %s", w.Code, w.Body.String())
	}
	t.Setenv("NEON_DSGA_SUSPENSION_ENABLED", "true")
	if w := post("resume", `"1"`, "same-resume-intent"); w.Code != 412 {
		t.Fatalf("stale version accepted: %d %s", w.Code, w.Body.String())
	}
	w := post("resume", `"2"`, "same-resume-intent")
	var op struct{ Operation struct{ ID, Action string } }
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &op) != nil || op.Operation.ID == "" || op.Operation.Action != dsgaResumeAction {
		t.Fatalf("resume was not queued: %d %s", w.Code, w.Body.String())
	}
	id := op.Operation.ID
	if w := post("resume", `"2"`, "same-resume-intent"); w.Code != 202 || !strings.HasSuffix(w.Header().Get("Location"), "/"+id) {
		t.Fatalf("replay changed the original Operation: %d %s", w.Code, w.Body.String())
	}
	if w := post("resume", `"2"`, "different-resume-key"); w.Code != 409 {
		t.Fatalf("a concurrent resume was accepted: %d %s", w.Code, w.Body.String())
	}
	if _, err := s.db.Exec(ctx, `UPDATE operations SET payload=payload-'resume_from' WHERE id=$1`, id); err == nil {
		t.Fatal("settled predecessor could be changed")
	}
	exec(`UPDATE operations SET state='failed',retryable=true WHERE id=$1`, id)
	if err := s.retryFailed(ctx, id, p.ProjectID); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE operations SET state='running',lease_owner=$2,lease_expires_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, id, b.workerID)
	next, err := s.loadDSGACreationBinding(ctx, id, b.workerID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := next.creationPlan(ctx)
	if err != nil || !plan.ResumeGeneration || plan.Credentials != previous.Credentials || plan.ComputeInstanceID != previous.ComputeInstanceID || next.intent.ResumeFrom != source.Operation {
		t.Fatalf("accepted intent changed the stable endpoint: %v", err)
	}
	if err := next.bind(ctx, "resume-vm-uid", ""); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO dsga_creation_credential_sources(operation_id,source_version,credentials_uid,content_digest)
 SELECT $1,source_version,credentials_uid,content_digest FROM dsga_creation_credential_sources WHERE operation_id=$2`, id, source.Creation)
	if err := next.bind(ctx, "resume-vm-uid", "resume-config-uid"); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO dsga_creation_services(operation_id,service_kind,namespace,service_name,vm_uid,service_uid)
 SELECT $1,service_kind,namespace,service_name,'resume-vm-uid','resume-'||service_uid FROM dsga_creation_services WHERE operation_id=$2`, id, source.Creation)
	if _, err = next.supplyFact(ctx, "runtime_selection", dsgaSupplyObject{runtimeselection.Name("resume-vm-uid"), "resume-selection-uid", strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	// These test receipts exercise the real database guards without booting a VM.
	fact.Observed.Instance.VMUID = "resume-vm-uid"
	fact.Observed.Instance.PGBootID++
	native, err := plan.nativeBootstrap(p, "resume-vm-uid")
	if err != nil {
		t.Fatal(err)
	}
	fact.Startup, fact.OperationUUID = native, plan.OperationUUID
	raw, err := json.Marshal(fact)
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO dsga_creation_initialization_observations(operation_id,pg_boot_id,vm_uid,config_uid,credentials_uid,management_service_uid,receipt)
 SELECT $1,$2,'resume-vm-uid','resume-config-uid',$3,service_uid,$4 FROM dsga_creation_services WHERE operation_id=$1 AND service_kind='management'`,
		id, fmt.Sprint(uint64(fact.Observed.Instance.PGBootID)), plan.Credentials.UID, raw)
	if e, err := s.dsgaEndpointRuntime(ctx, p.ProjectID, p.EndpointID); err != nil || e.Operation != source.Creation {
		t.Fatalf("incomplete successor displaced original runtime: %v", err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := lockDSGAProjectParents(ctx, tx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE operations SET state='succeeded',finished_at=now(),lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := commitDSGAResume(ctx, tx, id, p.ProjectID, p.EndpointID); !errors.Is(err, errDSGACreationIdentity) {
		t.Fatalf("promoted a resume before its steps: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE operation_steps SET state='succeeded' WHERE operation_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := commitDSGAResume(ctx, tx, id, p.ProjectID, p.EndpointID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if e, err := s.dsgaEndpointRuntime(ctx, p.ProjectID, p.EndpointID); err != nil || e.Operation != id || e.VMUID != "resume-vm-uid" {
		t.Fatalf("completed successor did not become authoritative: %v", err)
	}
	w = post("suspend", `"3"`, "next-suspension-key")
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &op) != nil || op.Operation.Action != dsgaSuspendAction {
		t.Fatalf("second suspension was not queued: %d %s", w.Code, w.Body.String())
	}
	var generation string
	if err := s.db.QueryRow(ctx, `SELECT payload->>'creation_operation_id' FROM operations WHERE id=$1`, op.Operation.ID).Scan(&generation); err != nil || generation != id {
		t.Fatalf("second suspension selected the previous VM: %v", err)
	}
}
