package control

import (
	"bytes"
	"context"
	"errors"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/bootstrap"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func renderDSGAService(vm *vmv1.VirtualMachine, p createPayload, plan dsgaCreationPlan, operation, kind string) (*core.Service, error) {
	if vm == nil || vm.Name != kubeName(p.EndpointID) || vm.Namespace != plan.Namespace || dsgapi.ValidateID(string(vm.UID), "VM") != nil ||
		plan.validate(p) != nil || dsgapi.ValidateID(operation, "creation Operation") != nil || (kind != "postgres" && kind != "management") {
		return nil, errDSGACreationIdentity
	}
	name, port, portName := vm.Name, int32(55433), "postgres"
	if kind == "management" {
		name, port, portName = vm.Name+"-management", 3080, "compute-control"
	}
	controller, blocking := true, false
	return &core.Service{ObjectMeta: metav1.ObjectMeta{Namespace: vm.Namespace, Name: name,
		Labels:          credentialLabels(p.ProjectID, p.EndpointID),
		Annotations:     map[string]string{dsgaCredentialOperation: operation, dsgaCredentialIncarnation: plan.Incarnation, dsgaCredentialCompute: plan.ComputeInstanceID},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: vmv1.SchemeGroupVersion.String(), Kind: "VirtualMachine", Name: vm.Name, UID: vm.UID, Controller: &controller, BlockOwnerDeletion: &blocking}}},
		Spec: core.ServiceSpec{Type: core.ServiceTypeClusterIP, SessionAffinity: core.ServiceAffinityNone,
			Selector: map[string]string{"vm.neon.tech/name": vm.Name}, Ports: []core.ServicePort{{Name: portName, Protocol: core.ProtocolTCP, Port: port, TargetPort: intstr.FromInt32(port)}}}}, nil
}

// API分配的ClusterIP/IPFamilies可按平台默认产生，其余路由语义必须与原构造
// 相同。仅标签或同名不足以收养；必须同一Operation/incarnation/Compute和VM
// controller UID。NodePort/外部IP/额外selector、端口、亲和或提前发布均拒绝，
// 不PATCH修复漂移对象。此检查只证明路由形状，不证明Guest/SQL/预算已就绪。
func sameDSGAService(actual, wanted *core.Service, expectedUID string) bool {
	if actual == nil || wanted == nil || actual.Namespace != wanted.Namespace || actual.Name != wanted.Name || dsgapi.ValidateID(string(actual.UID), "Service") != nil ||
		actual.ResourceVersion == "" || actual.DeletionTimestamp != nil || (expectedUID != "" && string(actual.UID) != expectedUID) || len(actual.OwnerReferences) != 1 {
		return false
	}
	for _, key := range []string{"neon-control/project-id", "neon-control/endpoint-id"} {
		if actual.Labels[key] != wanted.Labels[key] {
			return false
		}
	}
	for key, value := range wanted.Annotations {
		if actual.Annotations[key] != value {
			return false
		}
	}
	owner, original := actual.OwnerReferences[0], wanted.OwnerReferences[0]
	if owner.APIVersion != original.APIVersion || owner.Kind != original.Kind || owner.Name != original.Name || owner.UID != original.UID || owner.Controller == nil || !*owner.Controller ||
		(owner.BlockOwnerDeletion != nil && *owner.BlockOwnerDeletion) {
		return false
	}
	a, w := actual.Spec, wanted.Spec
	ip := net.ParseIP(a.ClusterIP)
	if a.Type != core.ServiceTypeClusterIP || ip == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || a.ExternalName != "" || len(a.ExternalIPs) != 0 || a.PublishNotReadyAddresses ||
		len(a.Selector) != 1 || a.Selector["vm.neon.tech/name"] != w.Selector["vm.neon.tech/name"] || len(a.Ports) != 1 ||
		(a.SessionAffinity != "" && a.SessionAffinity != core.ServiceAffinityNone) || a.SessionAffinityConfig != nil || a.HealthCheckNodePort != 0 ||
		a.LoadBalancerIP != "" || len(a.LoadBalancerSourceRanges) != 0 || a.LoadBalancerClass != nil || a.AllocateLoadBalancerNodePorts != nil ||
		(a.InternalTrafficPolicy != nil && *a.InternalTrafficPolicy != core.ServiceInternalTrafficPolicyCluster) || a.TrafficDistribution != nil ||
		(a.ExternalTrafficPolicy != "" && a.ExternalTrafficPolicy != core.ServiceExternalTrafficPolicyCluster) {
		return false
	}
	p, target := a.Ports[0], w.Ports[0]
	return p.Name == target.Name && p.Protocol == target.Protocol && p.Port == target.Port && p.TargetPort == target.TargetPort && p.NodePort == 0 && p.AppProtocol == nil
}

// Operation→原VM/Config绑定→Service行。不跨网络持锁；首次固定后，同一来源
// 恢复只读，连xmin/时间也不变。未知COMMIT交下一次原意图读取，不覆盖旧UID。
func (binding *dsgaCreationBinding) serviceIdentity(ctx context.Context, kind string, observed *core.Service) (string, error) {
	tx, err := binding.lockOperation(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	vm, config, err := binding.read(ctx, tx)
	if err != nil || vm == "" || config == "" || (kind != "postgres" && kind != "management") {
		return "", errDSGACreationIdentity
	}
	var uid string
	err = tx.QueryRow(ctx, `SELECT service_uid FROM dsga_creation_services WHERE operation_id=$1 AND service_kind=$2 FOR SHARE`, binding.operationID, kind).Scan(&uid)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
		if observed != nil {
			_, err = tx.Exec(ctx, `INSERT INTO dsga_creation_services(operation_id,service_kind,namespace,service_name,vm_uid,service_uid) VALUES($1,$2,$3,$4,$5,$6)`, binding.operationID, kind, observed.Namespace, observed.Name, vm, string(observed.UID))
			uid = string(observed.UID)
		}
	} else if err == nil && observed != nil && uid != string(observed.UID) {
		err = errDSGACreationIdentity
	}
	if err != nil {
		return "", err
	}
	if err := binding.checkLease(ctx, tx); err != nil {
		return "", err
	}
	return uid, tx.Commit(ctx)
}

// prepareServices是原创建Driver的实际生产入口。先恢复同一原VM/Config/凭据
// 和完整journal，再逐个创建并固定两个内部Service；不接受外部模板或URL。
// Service在SQL路由发布前可准备，但PG普通准入仍关闭；这里不把Service存在
// 当作初始化/预算成功，不触碰legacy mutable模板或任意QMP/VM资源动作。
func (binding *dsgaCreationBinding) prepareServices(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	current, err := binding.server.loadDSGACreationBinding(ctx, binding.operationID, binding.workerID)
	if err != nil {
		return err
	}
	if current.vmUID == "" || current.configUID == "" {
		return errDSGACreationIdentity
	}
	plan, err := current.creationPlan(ctx)
	if err != nil {
		return err
	}
	clients, vms, err := newDSGAKubeClients(current.server.kube)
	if err != nil {
		return err
	}
	p := current.intent.createPayload
	vm, err := vms.NeonvmV1().VirtualMachines(plan.Namespace).Get(ctx, kubeName(p.EndpointID), metav1.GetOptions{})
	if err != nil || current.validateVM(vm) != nil || string(vm.UID) != current.vmUID {
		return errDSGACreationIdentity
	}
	wantedVM, err := renderDSGAVM(p, plan)
	if err != nil || !sameDSGAVM(vm, wantedVM) {
		return errDSGACreationIdentity
	}
	config, err := clients.CoreV1().Secrets(plan.Namespace).Get(ctx, vm.Name+"-config", metav1.GetOptions{})
	if err != nil || string(config.UID) != current.configUID || config.Immutable == nil || !*config.Immutable || bootstrap.CheckReady(vm, config, false) != nil {
		return errDSGACreationIdentity
	}
	data, err := current.renderDSGABootstrap(ctx, vm)
	if err != nil || len(config.Data) != 1 || !bytes.Equal(config.Data["config.json"], data["config.json"]) {
		return errDSGACreationIdentity
	}
	for _, kind := range []string{"postgres", "management"} {
		wanted, err := renderDSGAService(vm, p, plan, current.operationID, kind)
		if err != nil {
			return err
		}
		uid, err := current.serviceIdentity(ctx, kind, nil)
		if err != nil {
			return err
		}
		service, err := clients.CoreV1().Services(plan.Namespace).Get(ctx, wanted.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) && uid == "" {
			created := new(core.Service)
			// 一次POST，429/超时/丢回复不能透明重发。409只可GET同原赢家，
			// 未知结果交下一次原Operation恢复；已固定UID丢失绝不重建。
			err = clients.CoreV1().RESTClient().Post().Namespace(plan.Namespace).Resource("services").Body(wanted).MaxRetries(0).Do(ctx).Into(created)
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return err
			}
			conflict := err != nil
			service, err = clients.CoreV1().Services(plan.Namespace).Get(ctx, wanted.Name, metav1.GetOptions{})
			if err == nil && !conflict && service.UID != created.UID {
				return errDSGACreationIdentity
			}
		}
		if err != nil {
			return err
		}
		if !sameDSGAService(service, wanted, uid) {
			return errDSGACreationIdentity
		}
		if _, err := current.serviceIdentity(ctx, kind, service); err != nil {
			return err
		}
	}
	return nil
}
