package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	"github.com/neondatabase/autoscaling/pkg/dsga/qualificationissuer"
	"github.com/neondatabase/autoscaling/pkg/dsga/runtimeselection"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// 在线策略只读原创建发行/容量，不重新选镜像，不修改VM几何或启动配置。
type dsgaEndpointRuntime struct {
	Operation        string
	Payload          createPayload
	Plan             dsgaCreationPlan
	VMUID, ConfigUID string
}

func (s *server) dsgaEndpointRuntime(ctx context.Context, project, endpoint string) (*dsgaEndpointRuntime, error) {
	rows, err := s.db.Query(ctx, `SELECT o.id,o.payload,b.vm_uid,COALESCE(b.config_uid,'')
 FROM operations o LEFT JOIN dsga_creation_bindings b ON b.operation_id=o.id
 WHERE o.project_id=$1 AND o.payload->>'endpoint_id'=$2
 AND o.action IN ('create_project_dsga_v1','create_branch_dsga_v1','create_endpoint_dsga_v1')
 ORDER BY o.created_at DESC LIMIT 2`, project, endpoint)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result *dsgaEndpointRuntime
	for rows.Next() {
		if result != nil {
			return nil, errDSGACreationIdentity
		}
		var raw []byte
		var id string
		var vm, cfg *string
		if err = rows.Scan(&id, &raw, &vm, &cfg); err != nil {
			return nil, err
		}
		var envelope struct {
			createPayload
			Plan dsgaCreationPlan `json:"dsga_creation"`
		}
		if decodeDSGAObject(raw, &envelope) != nil || envelope.Plan.validate(envelope.createPayload) != nil || envelope.ProjectID != project || envelope.EndpointID != endpoint {
			return nil, errDSGACreationIdentity
		}
		result = &dsgaEndpointRuntime{Operation: id, Payload: envelope.createPayload, Plan: envelope.Plan}
		if vm != nil {
			result.VMUID = *vm
		}
		if cfg != nil {
			result.ConfigUID = *cfg
		}
	}
	return result, rows.Err()
}
func (e *dsgaEndpointRuntime) binding() journal.Binding {
	return journal.Binding{Namespace: e.Plan.Namespace, Name: kubeName(e.Payload.EndpointID), VMUID: types.UID(e.VMUID), Incarnation: e.Plan.Incarnation}
}
func (e *dsgaEndpointRuntime) validateBounds(p boundsPayload) error {
	s := e.Plan.VM.supply()
	if s == nil || p.ProjectID != e.Payload.ProjectID || p.EndpointID != e.Payload.EndpointID || p.WorkloadName != kubeName(e.Payload.EndpointID) ||
		p.MinCPU != e.Payload.MinCPU || p.MaxCPU != e.Payload.MaxCPU || p.MinMem < e.Payload.MinMem || p.MaxMem > e.Payload.MaxMem || p.MinMem > p.MaxMem {
		return errDSGAProjectBounds
	}
	automatic := s.clone().Automatic
	automatic.MinimumVMMemoryBytes = dsgapi.Uint64(p.MinMem) << 20
	automatic.MaximumVMMemoryBytes = dsgapi.Uint64(p.MaxMem) << 20
	if uint64(automatic.MinimumVMMemoryBytes)%e.Plan.MemorySlotBytes != 0 || uint64(automatic.MaximumVMMemoryBytes)%e.Plan.MemorySlotBytes != 0 || automatic.Validate() != nil {
		return errDSGAProjectBounds
	}
	return nil
}
func (s *server) readDSGASelection(ctx context.Context, e *dsgaEndpointRuntime) (kubernetes.Interface, *vmv1.VirtualMachine, *core.ConfigMap, runtimeselection.Selection, error) {
	clients, vm, cm, selected, err := s.readDSGABoundSelection(ctx, e)
	if err == nil && selected.Automatic == nil {
		err = errDSGACreationIdentity
	}
	return clients, vm, cm, selected, err
}

func (s *server) readDSGABoundSelection(ctx context.Context, e *dsgaEndpointRuntime) (kubernetes.Interface, *vmv1.VirtualMachine, *core.ConfigMap, runtimeselection.Selection, error) {
	var empty runtimeselection.Selection
	if e == nil || e.Plan.VM.supply() == nil || e.VMUID == "" || e.ConfigUID == "" {
		return nil, nil, nil, empty, errDSGACreationIdentity
	}
	clients, vms, err := newDSGAKubeClients(s.kube)
	if err != nil {
		return nil, nil, nil, empty, err
	}
	vm, err := vms.NeonvmV1().VirtualMachines(e.Plan.Namespace).Get(ctx, e.binding().Name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, nil, empty, err
	}
	wanted, err := renderDSGAVM(e.Payload, e.Plan)
	if err != nil || vm.UID != types.UID(e.VMUID) || vm.DeletionTimestamp != nil || vm.Status.Phase != vmv1.VmRunning || vm.Spec.DSGABootstrap == nil ||
		!vm.Spec.DSGABootstrap.Ready || vm.Spec.DSGABootstrap.ConfigSecretUID != e.ConfigUID || !sameDSGAVM(vm, wanted) {
		return nil, nil, nil, empty, errDSGACreationIdentity
	}
	var raw, proof []byte
	if err = s.db.QueryRow(ctx, "SELECT value FROM dsga_creation_supply_facts WHERE operation_id=$1 AND kind='runtime_selection'", e.Operation).Scan(&raw); err != nil {
		return nil, nil, nil, empty, err
	}
	var ref dsgaSupplyObject
	if json.Unmarshal(raw, &ref) != nil {
		return nil, nil, nil, empty, errDSGACreationIdentity
	}
	cm, err := clients.CoreV1().ConfigMaps(e.Plan.VM.supply().PlatformNamespace).Get(ctx, runtimeselection.Name(e.VMUID), metav1.GetOptions{})
	if err != nil {
		return nil, nil, nil, empty, err
	}
	selected, err := runtimeselection.Decode(cm, e.binding(), e.Plan.VM.supply().PlatformNamespace)
	if err != nil || string(cm.UID) != ref.UID || selected.Instance.ComputeInstanceID != e.Plan.ComputeInstanceID {
		return nil, nil, nil, empty, errDSGACreationIdentity
	}
	if err = s.db.QueryRow(ctx, "SELECT value FROM dsga_creation_supply_facts WHERE operation_id=$1 AND kind='qualification_result'", e.Operation).Scan(&proof); err != nil {
		return nil, nil, nil, empty, err
	}
	var q qualificationissuer.Result
	if json.Unmarshal(proof, &q) != nil || q.Source != selected.Qualification {
		return nil, nil, nil, empty, errDSGACreationIdentity
	}
	return clients, vm, cm, selected, nil
}

type dsgaPolicyIntent struct {
	SelectionUID   string                     `json:"selection_uid"`
	PreviousSHA256 string                     `json:"previous_sha256"`
	Next           runtimeselection.Selection `json:"next"`
}

// 幂等键仍是原公开Operation；只冻结期望代次，不另造资源账本或执行器。
func (s *server) dsgaPolicyIntent(ctx context.Context, id, worker string, e *dsgaEndpointRuntime, p boundsPayload, cm *core.ConfigMap, current runtimeselection.Selection) (dsgaPolicyIntent, error) {
	var intent dsgaPolicyIntent
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return intent, err
	}
	defer tx.Rollback(ctx)
	if err = assertCreateLeaseTx(ctx, tx, id, worker); err != nil {
		return intent, err
	}
	var action, project, endpoint string
	var original []byte
	if err = tx.QueryRow(ctx, "SELECT action,project_id,resource_id,payload FROM operations WHERE id=$1 FOR UPDATE", id).Scan(&action, &project, &endpoint, &original); err != nil {
		return intent, err
	}
	var expected boundsPayload
	if json.Unmarshal(original, &expected) != nil || expected != p || action != "update_endpoint" || project != e.Payload.ProjectID || endpoint != e.Payload.EndpointID {
		return intent, errDSGACreationIdentity
	}
	var raw []byte
	err = tx.QueryRow(ctx, "SELECT intent FROM dsga_runtime_policy_intents WHERE operation_id=$1", id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		if current.Generation == ^dsgapi.Uint64(0) {
			return intent, errDSGACreationIdentity
		}
		next := current
		a := e.Plan.VM.supply().clone().Automatic
		a.MinimumVMMemoryBytes = dsgapi.Uint64(p.MinMem) << 20
		a.MaximumVMMemoryBytes = dsgapi.Uint64(p.MaxMem) << 20
		next.Automatic = &a
		next.Generation++
		intent = dsgaPolicyIntent{string(cm.UID), current.Digest(), next}
		raw, err = json.Marshal(intent)
		if err == nil {
			_, err = tx.Exec(ctx, "INSERT INTO dsga_runtime_policy_intents(operation_id,intent) VALUES($1,$2)", id, raw)
		}
	} else if err == nil {
		err = json.Unmarshal(raw, &intent)
	}
	if err != nil {
		return intent, err
	}
	if err = assertCreateLeaseTx(ctx, tx, id, worker); err != nil {
		return intent, err
	}
	return intent, tx.Commit(ctx)
}
func (s *server) reconcileEndpointBounds(ctx context.Context, id, worker string, p boundsPayload) error {
	endpoint, err := s.dsgaEndpointRuntime(ctx, p.ProjectID, p.EndpointID)
	if err != nil {
		return err
	}
	if endpoint == nil {
		return s.kube.reconcileVMBounds(ctx, p)
	}
	if err = endpoint.validateBounds(p); err != nil {
		return err
	}
	clients, vm, cm, current, err := s.readDSGASelection(ctx, endpoint)
	if err != nil {
		return err
	}
	intent, err := s.dsgaPolicyIntent(ctx, id, worker, endpoint, p, cm, current)
	if err != nil {
		return err
	}
	if err = writeDSGASelection(ctx, clients, cm, current, intent); err != nil {
		return err
	}
	desired := intent.Next.Digest()
	wait, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		state, err := readDSGAOwnerPolicy(wait, clients, vm, endpoint.Plan.VM.supply(), intent.Next)
		if err == nil && state.AppliedGeneration == intent.Next.Generation && state.AppliedSHA256 == desired {
			return nil
		}
		if waitDSGAStartupPoll(wait) != nil {
			return errors.New("original Owner has not confirmed the selected policy generation")
		}
	}
}

func writeDSGASelection(ctx context.Context, clients kubernetes.Interface, cm *core.ConfigMap, current runtimeselection.Selection, intent dsgaPolicyIntent) error {
	if string(cm.UID) != intent.SelectionUID {
		return errDSGACreationIdentity
	}
	currentDigest := current.Digest()
	desired := intent.Next.Digest()
	if currentDigest == desired {
		return nil
	}
	if currentDigest != intent.PreviousSHA256 || current.Instance != intent.Next.Instance || current.Qualification != intent.Next.Qualification ||
		current.Generation == ^dsgapi.Uint64(0) || current.Generation+1 != intent.Next.Generation || intent.Next.Validate() != nil {
		return errDSGACreationIdentity
	}
	raw, _ := json.Marshal(intent.Next)
	cm = cm.DeepCopy()
	cm.Data[runtimeselection.DataKey] = string(raw)
	updated := &core.ConfigMap{}
	// One resourceVersion CAS. A lost response is recovered by reading the same
	// ConfigMap UID and exact digest from the persisted intent.
	err := clients.CoreV1().RESTClient().Put().Namespace(cm.Namespace).Resource("configmaps").Name(cm.Name).Body(cm).MaxRetries(0).Do(ctx).Into(updated)
	if err != nil {
		return err
	}
	binding := journal.Binding{Namespace: current.Namespace, Name: current.Name, VMUID: types.UID(current.Instance.VMUID), Incarnation: current.Incarnation}
	applied, err := runtimeselection.Decode(updated, binding, cm.Namespace)
	if err != nil || updated.UID != cm.UID || applied.Digest() != desired {
		return errDSGACreationIdentity
	}
	return nil
}

// 上游Agent原dump-state仅证明策略采纳，真实PG/硬限另读。观察超时不停止SQL。
func readDSGAOwnerPolicy(ctx context.Context, clients kubernetes.Interface, vm *vmv1.VirtualMachine, p *dsgaSupplyProfile, selected runtimeselection.Selection) (runtimeselection.Status, error) {
	raw, err := readDSGAOwnerDump(ctx, clients, vm, p)
	if err != nil {
		return runtimeselection.Status{}, err
	}
	return decodeDSGAOwnerPolicy(raw, vm.Namespace, vm.Status.PodName, selected, time.Now())
}

func readDSGAOwnerDump(ctx context.Context, clients kubernetes.Interface, vm *vmv1.VirtualMachine, p *dsgaSupplyProfile) ([]byte, error) {
	raw, _, err := readDSGAOwnerPodDump(ctx, clients, vm, p)
	return raw, err
}

func readDSGAOwnerPodDump(ctx context.Context, clients kubernetes.Interface, vm *vmv1.VirtualMachine, p *dsgaSupplyProfile) ([]byte, types.UID, error) {
	ds, err := clients.AppsV1().DaemonSets(p.AgentNamespace).Get(ctx, p.AgentDaemonSet, metav1.GetOptions{})
	if err != nil {
		return nil, "", err
	}
	pods, err := clients.CoreV1().Pods(p.AgentNamespace).List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + vm.Status.Node})
	if err != nil {
		return nil, "", err
	}
	var target *core.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		ref := metav1.GetControllerOf(pod)
		if pod.DeletionTimestamp == nil && pod.Status.Phase == core.PodRunning && pod.Spec.ServiceAccountName == p.AgentServiceAccount && ref != nil && ref.Kind == "DaemonSet" && ref.UID == ds.UID {
			if target != nil {
				return nil, "", errDSGACreationIdentity
			}
			target = pod
		}
	}
	if target == nil {
		return nil, "", errDSGAInitializationPending
	}
	// client-go removes a bare trailing slash. Use a path handled by the
	// Agent dump mux so Kubernetes forwards the GET body instead of redirecting.
	raw, err := clients.CoreV1().RESTClient().Get().Namespace(p.AgentNamespace).Resource("pods").Name(target.Name+":"+strconv.Itoa(int(p.AgentDumpPort))).SubResource("proxy").Suffix("dump-state").SetHeader("Content-Type", "application/json").Body([]byte("{}")).DoRaw(ctx)
	if err != nil || len(raw) > 2<<20 {
		return nil, "", errDSGAInitializationPending
	}
	return raw, target.UID, nil
}

type dsgaOwnerObservation struct {
	Instance         dsgapi.Instance          `json:"instance"`
	IssuerEpoch      dsgapi.Uint64            `json:"issuerEpoch"`
	ObservedAt       time.Time                `json:"observedAt"`
	RuntimeSelection *runtimeselection.Status `json:"runtimeSelection"`
	SuspensionDrain  *dsgapi.PreparedResponse `json:"suspensionDrain,omitempty"`
}

func decodeDSGAOwnerPolicy(raw []byte, namespace, podName string, selected runtimeselection.Selection, now time.Time) (runtimeselection.Status, error) {
	value, err := decodeDSGAOwnerObservation(raw, namespace, podName, selected, now)
	if err != nil {
		return runtimeselection.Status{}, err
	}
	return *value.RuntimeSelection, nil
}

func decodeDSGAOwnerObservation(raw []byte, namespace, podName string, selected runtimeselection.Selection, now time.Time) (dsgaOwnerObservation, error) {
	var empty dsgaOwnerObservation
	var dump struct {
		Stopped *bool `json:"stopped"`
		Pods    []struct {
			PodName struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"podName"`
			Runner json.RawMessage `json:"runner"`
		} `json:"pods"`
	}
	if dsgapi.ValidateObservationJSON(raw, 2<<20, 65536) != nil || json.Unmarshal(raw, &dump) != nil || dump.Stopped == nil || *dump.Stopped {
		return empty, errDSGAInitializationPending
	}
	var matched *dsgaOwnerObservation
	for _, pod := range dump.Pods {
		if pod.PodName.Namespace != namespace || pod.PodName.Name != podName {
			continue
		}
		// Other Runners can legitimately have no PG boot during startup or
		// after retirement. Decode the strict instance only for the original Pod.
		var runner struct {
			DSGA dsgaOwnerObservation `json:"dsga"`
		}
		if matched != nil || json.Unmarshal(pod.Runner, &runner) != nil {
			return empty, errDSGAInitializationPending
		}
		d := runner.DSGA
		age := now.Sub(d.ObservedAt)
		// 此期限仅为页面/操作回执的新鲜度，绝不是数据库生存租约。
		if d.Instance != selected.Instance || d.RuntimeSelection == nil || age < -5*time.Second || age > 2*time.Minute {
			return empty, errDSGAInitializationPending
		}
		state := *d.RuntimeSelection
		if state.DesiredGeneration != selected.Generation || state.DesiredSHA256 != selected.Digest() {
			return empty, errDSGAInitializationPending
		}
		matched = &d
	}
	if matched == nil {
		return empty, errDSGAInitializationPending
	}
	return *matched, nil
}
func (s *server) validateDSGAEndpointBounds(ctx context.Context, project, endpoint string, b createBounds) (bool, error) {
	e, err := s.dsgaEndpointRuntime(ctx, project, endpoint)
	if err != nil {
		return true, err
	}
	if e == nil {
		return false, nil
	}
	return true, e.validateBounds(boundsPayload{project, endpoint, kubeName(endpoint), b.MinCPU, b.MaxCPU, b.MinMem, b.MaxMem})
}
func (s *server) endpointMemoryPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireEndpoint(w, r); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	e, err := s.dsgaEndpointRuntime(ctx, r.PathValue("project"), r.PathValue("endpoint"))
	if err != nil {
		fail(w, r, 503, "runtime_unavailable", "Original runtime identity unavailable")
		return
	}
	if e == nil || e.Plan.VM.supply() == nil {
		fail(w, r, 422, "capability_disabled", "PostgreSQL memory policy is not configured for this endpoint")
		return
	}
	clients, vm, _, selected, err := s.readDSGASelection(ctx, e)
	if err != nil {
		fail(w, r, 503, "runtime_unavailable", "Original runtime selection unavailable")
		return
	}
	state, err := readDSGAOwnerPolicy(ctx, clients, vm, e.Plan.VM.supply(), selected)
	if err != nil {
		fail(w, r, 503, "policy_unconfirmed", "Original Owner policy observation is not current")
		return
	}
	actual, err := s.observeDSGAEndpointMemory(ctx, e, vm, selected)
	if err != nil {
		fail(w, r, 503, "memory_unconfirmed", "Original PostgreSQL and hard-limit observation unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, 200, map[string]any{"protocol_version": 1, "postgres_version": e.Plan.PostgresVersion, "desired_generation": selected.Generation,
		"applied_generation": state.AppliedGeneration, "state": state.State, "desired": map[string]any{"min_memory_mib": uint64(selected.Automatic.MinimumVMMemoryBytes) >> 20, "max_memory_mib": uint64(selected.Automatic.MaximumVMMemoryBytes) >> 20}, "actual": actual})
}
