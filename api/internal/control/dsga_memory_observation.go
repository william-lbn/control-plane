package control

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"time"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	"github.com/neondatabase/autoscaling/pkg/dsga/resourceguard"
	"github.com/neondatabase/autoscaling/pkg/dsga/runtimeselection"
	"github.com/william-lbn/control-plane/api/internal/adapter"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// 公开状态仅投影本次真实GET的物理值，不把metadata、Pod Ready或applied代次
// 当作内存完成；中途代次变化返回未确认，下一次正常查询再观察。
type dsgaMemoryActual struct {
	ObservedAt         time.Time     `json:"observed_at"`
	VMMemoryBytes      dsgapi.Uint64 `json:"vm_memory_bytes"`
	SharedBufferBytes  dsgapi.Uint64 `json:"shared_buffer_bytes"`
	WorkMemoryMaxBytes dsgapi.Uint64 `json:"work_memory_max_bytes"`
	CapacityGeneration dsgapi.Uint64 `json:"capacity_generation"`
	AdmissionOpen      bool          `json:"sql_admission_open"`
}

func (s *server) observeDSGAEndpointMemory(ctx context.Context, e *dsgaEndpointRuntime, vm *vmv1.VirtualMachine, selected runtimeselection.Selection) (dsgaMemoryActual, error) {
	client := newDSGAProxy(nil).observer
	defer client.CloseIdleConnections()
	return s.observeDSGAEndpointMemoryWithHTTP(ctx, e, vm, selected, client)
}

// 与原初始化观察共用私有HTTP测试边界；生产地址仍只从原Service读取。
func (s *server) observeDSGAEndpointMemoryWithHTTP(ctx context.Context, e *dsgaEndpointRuntime, vm *vmv1.VirtualMachine, selected runtimeselection.Selection, client *http.Client) (dsgaMemoryActual, error) {
	var empty dsgaMemoryActual
	clients, vms, err := newDSGAKubeClients(s.kube)
	if err != nil {
		return empty, err
	}
	name := e.binding().Name
	cfg, err := clients.CoreV1().Secrets(e.Plan.Namespace).Get(ctx, name+"-config", metav1.GetOptions{})
	if err != nil {
		return empty, err
	}
	if string(cfg.UID) != e.ConfigUID || cfg.Immutable == nil || !*cfg.Immutable {
		return empty, errDSGACreationIdentity
	}
	credentials, err := clients.CoreV1().Secrets(e.Plan.Namespace).Get(ctx, name+"-credentials", metav1.GetOptions{})
	if err != nil {
		return empty, err
	}
	snap, err := snapshotDSGACredentials(credentials, e.Plan.Namespace, name+"-credentials", e.Payload.ProjectID, e.Payload.EndpointID)
	if err != nil || snap.reference() != e.Plan.Credentials {
		return empty, errDSGACreationIdentity
	}
	var serviceUID, sqlUID string
	if err = s.db.QueryRow(ctx, "SELECT service_uid FROM dsga_creation_services WHERE operation_id=$1 AND service_kind='management' AND vm_uid=$2", e.Operation, e.VMUID).Scan(&serviceUID); err != nil {
		return empty, err
	}
	if err = s.db.QueryRow(ctx, "SELECT service_uid FROM dsga_creation_services WHERE operation_id=$1 AND service_kind='postgres' AND vm_uid=$2", e.Operation, e.VMUID).Scan(&sqlUID); err != nil {
		return empty, err
	}
	in := managedSQLRouteInput{ProtocolVersion: 1, ManagementServiceUID: serviceUID, Binding: adapter.ManagedSQLBinding{ProtocolVersion: 1, Namespace: e.Plan.Namespace, ProjectID: e.Payload.ProjectID,
		EndpointID: e.Payload.EndpointID, BranchID: e.Payload.BranchID, TenantID: e.Payload.TenantID, TimelineID: e.Payload.TimelineID,
		VMUID: e.VMUID, Incarnation: e.Plan.Incarnation, ComputeInstanceID: e.Plan.ComputeInstanceID, ConfigSecretUID: e.ConfigUID,
		ConfigSHA256: managedSQLDigest(cfg.Data["config.json"]), SecretDataSHA256: managedSQLDataDigest(cfg.Data), CredentialSecretUID: string(credentials.UID), CredentialDataSHA256: managedSQLDataDigest(credentials.Data), ServiceUID: sqlUID}}
	source, err := readManagedSQLRouteSource(ctx, s.kube, in)
	if err != nil {
		return empty, err
	}
	before, err := observeManagedSQLNative(ctx, client, in, source)
	if err != nil || before.validate(e.Payload, e.Plan, e.VMUID) != nil || before.Observed.Instance != selected.Instance {
		return empty, errDSGACreationIdentity
	}
	log, err := journal.Inspect(source.vm, e.binding())
	if err != nil {
		return empty, err
	}
	epoch := log.State().Owner.Epoch
	if epoch == 0 || before.Observed.IssuerFloorEpoch != epoch {
		return empty, errDSGACreationIdentity
	}
	key := ed25519.NewKeyFromSeed(credentials.Data["controlSigningSeed"])
	now := time.Now()
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": computeKeyID(key)})
	claims, _ := json.Marshal(dsgaProxyClaims{Audience: "neon-dsga", Scope: "compute_ctl:dsga", ComputeID: name, VMUID: e.VMUID, ComputeInstanceID: e.Plan.ComputeInstanceID, IssuerEpoch: epoch, NotBefore: uint64(now.Add(-time.Second).Unix()), Expires: uint64(now.Add(time.Minute).Unix())})
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	token := signing + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(signing)))
	// 路径/方法固定于只读入口，私有令牌不进入公开响应、日志或持久状态。
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(source.management.Spec.ClusterIP, "3080")+"/dsg/v1/resource/state", nil)
	if err != nil {
		return empty, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	response, err := client.Do(req)
	if err != nil {
		return empty, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	media, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	var root resourceguard.View
	if err != nil || response.StatusCode != 200 || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Encoding") != "" ||
		mediaErr != nil || media != "application/json" || len(response.Header.Values("Content-Type")) != 1 || len(raw) > 1<<20 || dsgapi.ValidateObservationJSON(raw, 1<<20, 65536) != nil || json.Unmarshal(raw, &root) != nil || root.Validate() != nil || root.Identity.VMUID != e.VMUID || root.Identity.ComputeInstanceID != e.Plan.ComputeInstanceID || root.IssuerFloorEpoch != epoch {
		return empty, errDSGACreationIdentity
	}
	limits, err := root.ConfirmedLimits()
	if err != nil {
		return empty, err
	}
	after, err := observeManagedSQLNative(ctx, client, in, source)
	if err != nil || after.Observed.Instance != before.Observed.Instance || after.Observed.CapacityGeneration != before.Observed.CapacityGeneration ||
		after.Observed.LowBuffers != before.Observed.LowBuffers || after.Observed.HighBuffers != before.Observed.HighBuffers || after.Observed.IssuerFloorEpoch != epoch {
		return empty, errDSGACreationIdentity
	}
	latest, err := vms.NeonvmV1().VirtualMachines(e.Plan.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return empty, err
	}
	if latest.UID != vm.UID || latest.Status.PodName != vm.Status.PodName || latest.Status.Phase != vmv1.VmRunning || latest.Status.MemorySize == nil || vm.Status.MemorySize == nil ||
		latest.Status.MemorySize.Cmp(*vm.Status.MemorySize) != 0 || latest.Spec.Guest.MemorySlots.Use != vm.Spec.Guest.MemorySlots.Use {
		return empty, errDSGACreationIdentity
	}
	lastJournal, journalErr := journal.Inspect(latest, e.binding())
	if journalErr != nil || lastJournal.State().Owner.Epoch != epoch {
		return empty, errDSGACreationIdentity
	}
	memory, exact := latest.Status.MemorySize.AsInt64()
	if !exact || memory <= 0 || after.Observed.LowBuffers != after.Observed.HighBuffers {
		return empty, errDSGACreationIdentity
	}
	return dsgaMemoryActual{time.Now().UTC(), dsgapi.Uint64(memory), dsgapi.Uint64(after.Observed.LowBuffers) * 8192, limits.MemoryMaxBytes, after.Observed.CapacityGeneration, after.Observed.AdmissionOpen}, nil
}
