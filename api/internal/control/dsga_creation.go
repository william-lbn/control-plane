package control

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/bootstrap"
	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// dsgaCreationInput来自已持久化的版本化Operation，不接受普通HTTP请求自报VM/
// 配置/预算。VM模板在Operation接受时固定随机incarnation/Compute ID；UID由
// apiserver分配后必须先通过Bind持久化到同一原Operation，再写journal/配置。
// Render只在该UID绑定后调用，必须从原计划和私有Secret产生确定性的完整配置，
// 校验原生启动预算、PG版本及角色隔离；不能按重试时env另选模式或随机新身份。
// 本执行器只完成创建屏障，不发放预算/物理缩容许可；API生产者/Renderer随后接线。
type dsgaCreationInput struct {
	Payload createPayload
	VM      *vmv1.VirtualMachine
	// 已绑定的原UID必须随恢复传入，404或同名重建均拒绝。第一次为空时，
	// Bind也必须使用原Operation/worker lease做一次CAS；不能实现成无条件覆盖。
	ExpectedVMUID types.UID
	// RequireExisting允许历史协议只GET并核对原对象；404时拒绝创建新VM。
	RequireExisting bool
	Bind            func(context.Context, *vmv1.VirtualMachine) error
	// 配置也在释放前由原Operation固定实际UID；恢复不能借同名新Secret换身份。
	ExpectedConfigUID types.UID
	BindConfig        func(context.Context, *core.Secret) error
	Render            func(context.Context, *vmv1.VirtualMachine) (map[string][]byte, error)
	BeforeRelease     func(context.Context, *vmv1.VirtualMachine) error
}

var errDSGACreationIdentity = errors.New("DSGA creation identity or immutable inputs differ")

func (input dsgaCreationInput) validate(namespace string) error {
	vm, p := input.VM, input.Payload
	if vm == nil || p.ProjectID == "" || p.EndpointID == "" || input.Bind == nil || input.BindConfig == nil || input.Render == nil ||
		vm.Name != kubeName(p.EndpointID) || vm.Namespace != namespace || vm.UID != "" || vm.ResourceVersion != "" || vm.DeletionTimestamp != nil ||
		!reflect.ValueOf(vm.Status).IsZero() || vm.Labels["neon-control/project-id"] != p.ProjectID || vm.Labels["neon-control/endpoint-id"] != p.EndpointID || dsgapi.Managed(vm.Annotations) {
		return errDSGACreationIdentity
	}
	b := vm.Spec.DSGABootstrap
	if b == nil || b.ProtocolVersion != 1 || b.Ready || b.VMUID != "" || b.ConfigSecretUID != "" || dsgapi.ValidateIncarnation(b.Incarnation) != nil || dsgapi.ValidateID(b.ComputeInstanceID, "Compute") != nil {
		return errDSGACreationIdentity
	}
	if (input.ExpectedConfigUID != "" && input.ExpectedVMUID == "") ||
		(input.ExpectedVMUID != "" && dsgapi.ValidateID(string(input.ExpectedVMUID), "VM") != nil) ||
		(input.ExpectedConfigUID != "" && dsgapi.ValidateID(string(input.ExpectedConfigUID), "config Secret") != nil) {
		return errDSGACreationIdentity
	}
	return nil
}

// creationVM验证实际API对象，而非仅看同名或labels。ready可以在恢复时读回，
// 但held必须没有任何Runner/Node/资源执行历史；不能收养旧运行VM或改变原绑定。
func (input dsgaCreationInput) creationVM(vm *vmv1.VirtualMachine) error {
	if vm == nil || vm.Name != input.VM.Name || vm.Namespace != input.VM.Namespace || vm.UID == "" || vm.ResourceVersion == "" || vm.DeletionTimestamp != nil ||
		(input.ExpectedVMUID != "" && vm.UID != input.ExpectedVMUID) ||
		vm.Labels["neon-control/project-id"] != input.Payload.ProjectID || vm.Labels["neon-control/endpoint-id"] != input.Payload.EndpointID {
		return errDSGACreationIdentity
	}
	b, wanted := vm.Spec.DSGABootstrap, input.VM.Spec.DSGABootstrap
	if b == nil || b.ProtocolVersion != wanted.ProtocolVersion || b.Incarnation != wanted.Incarnation || b.ComputeInstanceID != wanted.ComputeInstanceID {
		return errDSGACreationIdentity
	}
	held, err := vm.DSGABootstrapHeld()
	if err != nil || (held && !vm.DSGAUndispatched()) {
		return errDSGACreationIdentity
	}
	return nil
}

// creationConfig固定同VM controller ownerref与immutable=true。UID相同不能证明
// 内容相同，因此恢复也逐字节比较原Renderer结果，绝不PATCH坏配置“修复”现场。
// 目录服务/catalog变更必须独立投影，不能在同一个Secret里改安全启动字段。
func (input dsgaCreationInput) creationConfig(vm *vmv1.VirtualMachine, data map[string][]byte) (*core.Secret, error) {
	remaining := 1 << 20
	for key, value := range data {
		if len(key) > remaining || len(value) > remaining-len(key) {
			return nil, errDSGACreationIdentity
		}
		remaining -= len(key) + len(value)
	}
	var identity dsgaCredentialConfig
	if len(data["config.json"]) == 0 || json.Unmarshal(data["config.json"], &identity) != nil || identity.Spec.Dynamic == nil ||
		!dsgapi.SupportedNativeBootstrapVersion(identity.Spec.Dynamic.ProtocolVersion) || identity.Spec.Dynamic.VMUID != string(vm.UID) || identity.Spec.Dynamic.ComputeInstanceID != vm.Spec.DSGABootstrap.ComputeInstanceID {
		return nil, errDSGACreationIdentity
	}
	controller, blocking, immutable := true, false, true
	return &core.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: vm.Namespace, Name: vm.Name + "-config",
		Labels:          credentialLabels(input.Payload.ProjectID, input.Payload.EndpointID),
		OwnerReferences: []metav1.OwnerReference{{APIVersion: vmv1.SchemeGroupVersion.String(), Kind: "VirtualMachine", Name: vm.Name, UID: vm.UID, Controller: &controller, BlockOwnerDeletion: &blocking}}},
		Type: core.SecretTypeOpaque, Immutable: &immutable, Data: data}, nil
}

func (input dsgaCreationInput) confirmConfig(vm *vmv1.VirtualMachine, actual, desired *core.Secret) error {
	if !dsgaOwnedSecret(actual, vm.Namespace, vm.Name+"-config", input.Payload.ProjectID, input.Payload.EndpointID) ||
		(input.ExpectedConfigUID != "" && actual.UID != input.ExpectedConfigUID) ||
		actual.Immutable == nil || !*actual.Immutable || actual.Type != core.SecretTypeOpaque || len(actual.OwnerReferences) != 1 ||
		!reflect.DeepEqual(actual.Data, desired.Data) {
		return errDSGACreationIdentity
	}
	ref := actual.OwnerReferences[0]
	if ref.APIVersion != vmv1.SchemeGroupVersion.String() || ref.Kind != "VirtualMachine" || ref.Name != vm.Name || ref.UID != vm.UID || ref.Controller == nil || !*ref.Controller {
		return errDSGACreationIdentity
	}
	return nil
}

// prepareDSGACreation是可信Driver的真实typed API执行器：CREATE held→持久UID→
// 完整初始journal→确定性immutable配置→UID/RV一次释放。只返回API确认的事实，
// 不启动Runner、不改内存、不透明重试POST/PATCH；失败时保留held与已有原事实。
// 丢回复由上层原Operation显式恢复：重读原UID/身份/完整日志/原配置，不新建一套
// 成功回执。读取、渲染、Bind不持本地锁；总期限30秒，每个typed API最多5秒。
func (k *kubeClient) prepareDSGACreation(ctx context.Context, input dsgaCreationInput) (*vmv1.VirtualMachine, error) {
	if err := input.validate(k.namespace); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	clients, vms, err := newDSGAKubeClients(k)
	if err != nil {
		return nil, err
	}
	vmAPI := vms.NeonvmV1().VirtualMachines(k.namespace)
	secretAPI := clients.CoreV1().Secrets(k.namespace)
	vm, err := vmAPI.Get(ctx, input.VM.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if input.ExpectedVMUID != "" {
			return nil, errDSGACreationIdentity
		}
		if input.RequireExisting {
			return nil, errDSGACreationPlan
		}
		// 有旧配置却没有VM时不重建新UID，避免从孤儿配置/旧实例恢复出新身份。
		_, secretErr := secretAPI.Get(ctx, input.VM.Name+"-config", metav1.GetOptions{})
		if !apierrors.IsNotFound(secretErr) {
			return nil, errDSGACreationIdentity
		}
		// typed Create默认可按Retry-After重试429；创建原身份的一次写入禁止
		// 透明重发。未知结果必须交下一次原Operation GET，不能生成新UID。
		created := new(vmv1.VirtualMachine)
		err = vms.NeonvmV1().RESTClient().Post().Namespace(k.namespace).Resource("virtualmachines").Body(input.VM.DeepCopy()).MaxRetries(0).Do(ctx).Into(created)
		vm = created
	}
	if err != nil {
		return nil, err
	}
	if err := input.creationVM(vm); err != nil {
		return nil, err
	}
	if err := input.Bind(ctx, vm.DeepCopy()); err != nil {
		return nil, err
	}
	store, err := journal.NewStore(vmAPI, journal.Binding{Namespace: vm.Namespace, Name: vm.Name, VMUID: vm.UID, Incarnation: vm.Spec.DSGABootstrap.Incarnation})
	if err != nil {
		return nil, err
	}
	var snapshot *journal.Snapshot
	if dsgapi.Managed(vm.Annotations) {
		// 已有任一journal标记时只读原日志；损坏/缺失不能退回Initialize。
		snapshot, err = store.Load(ctx)
	} else {
		// NeonVM Controller会在CREATE后异步添加Finalizer并推进resourceVersion。
		// 不能用CREATE响应中的旧RV初始化journal：先重新GET同一UID，再核对
		// 完整不可变VM Spec、原incarnation、held与undispatched状态。仅允许
		// Finalizer/metadata等不改变Spec的RV推进；每次Initialize仍由Store用
		// 最新RV执行JSON Patch CAS。若CAS窗口内又有安全metadata更新，则重读
		// 并重新验证，最多三次；未知写入若已留下journal则只Load确认，绝不覆盖。
		const maxJournalInitializeAttempts = 3
		for attempt := 0; attempt < maxJournalInitializeAttempts; attempt++ {
			latest, readErr := vmAPI.Get(ctx, input.VM.Name, metav1.GetOptions{})
			if readErr != nil {
				return nil, readErr
			}
			if latest.UID != vm.UID || latest.DeletionTimestamp != nil || !sameDSGAVM(latest, input.VM) {
				return nil, errDSGACreationIdentity
			}
			if dsgapi.Managed(latest.Annotations) {
				snapshot, err = store.Load(ctx)
				if err == nil && !journal.PristineBootstrap(snapshot.State()) {
					return nil, errDSGACreationIdentity
				}
				break
			}
			held, holdErr := latest.DSGABootstrapHeld()
			if holdErr != nil || !held || latest.Spec.DSGABootstrap == nil ||
				latest.Spec.DSGABootstrap.Ready || !latest.DSGAUndispatched() {
				return nil, errDSGACreationIdentity
			}
			bootstrapVersion := latest.ResourceVersion
			snapshot, err = store.Initialize(ctx, bootstrapVersion, "bootstrap-"+latest.Spec.DSGABootstrap.Incarnation)
			if err == nil {
				break
			}

			// Initialize返回错误后先确认是否为未知写入。若原journal已持久化，
			// 只接受同UID且PristineBootstrap的原日志；不再次写入或重置。
			after, readErr := vmAPI.Get(ctx, input.VM.Name, metav1.GetOptions{})
			if readErr != nil {
				return nil, err
			}
			if after.UID != vm.UID || after.DeletionTimestamp != nil ||
				!sameDSGAVM(after, input.VM) || !after.DSGAUndispatched() {
				return nil, errDSGACreationIdentity
			}
			if dsgapi.Managed(after.Annotations) {
				// PATCH响应未知时不在同一次调用内把读回事实冒充成功；将确认交给
				// 下一次原Operation恢复入口，保持UnconfirmedWrite的既有语义。
				return nil, err
			}
			// 没有journal且RV未变化时，错误不是Finalizer竞态，原样失败。
			// RV已变化时，下一轮会以新GET再次核对完整Spec和held屏障。
			if after.ResourceVersion == bootstrapVersion || attempt == maxJournalInitializeAttempts-1 {
				return nil, err
			}
			vm = after
		}
	}
	if err != nil {
		return nil, err
	}
	if snapshot == nil {
		return nil, errDSGACreationIdentity
	}
	vm = snapshot.VirtualMachine()
	if !vm.Spec.DSGABootstrap.Ready && !journal.PristineBootstrap(snapshot.State()) {
		return nil, errDSGACreationIdentity
	}
	data, err := input.Render(ctx, vm.DeepCopy())
	if err != nil {
		return nil, err
	}
	desired, err := input.creationConfig(vm, data)
	if err != nil {
		return nil, err
	}
	secret, err := secretAPI.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if vm.Spec.DSGABootstrap.Ready || input.ExpectedConfigUID != "" {
			return nil, errDSGACreationIdentity
		}
		created := new(core.Secret)
		err = clients.CoreV1().RESTClient().Post().Namespace(k.namespace).Resource("secrets").Body(desired).MaxRetries(0).Do(ctx).Into(created)
		secret = created
		if err != nil {
			return nil, err
		}
		// 成功CREATE也必须读回同UID的持久对象；未知写入不走这里，交原意图
		// 下一次GET确认。不能在一次重读中跟随同名重建并换用另一Secret UID。
		createdUID := secret.UID
		secret, err = secretAPI.Get(ctx, desired.Name, metav1.GetOptions{})
		if err == nil && secret.UID != createdUID {
			return nil, errDSGACreationIdentity
		}
	}
	if err != nil {
		return nil, err
	}
	if err := input.confirmConfig(vm, secret, desired); err != nil {
		return nil, err
	}
	if err := input.BindConfig(ctx, secret.DeepCopy()); err != nil {
		return nil, err
	}
	configUID := secret.UID
	if input.BeforeRelease != nil {
		if err := input.BeforeRelease(ctx, vm); err != nil {
			return nil, err
		}
	}
	if !vm.Spec.DSGABootstrap.Ready {
		// 只用同原绑定当前完整快照CAS；普通finalizer/RV更新可安全造成冲突，
		// 下一次原Operation重读处理，不能去掉RV或反复PATCH直到“看似成功”。
		snapshot, err = store.ReleaseBootstrap(ctx, snapshot.ResourceVersion(), secret.UID)
		if err != nil {
			return nil, err
		}
		vm = snapshot.VirtualMachine()
	}
	// Render/Secret API期间可能发生删除或同名重建。返回前再用原Store绑定
	// 直接读完整事实；这仍不是跨对象原子许可，Controller/最终Runner还须自验。
	snapshot, err = store.Load(ctx)
	if err != nil {
		return nil, err
	}
	vm = snapshot.VirtualMachine()
	secret, err = secretAPI.Get(ctx, desired.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if secret.UID != configUID || input.confirmConfig(vm, secret, desired) != nil {
		return nil, errDSGACreationIdentity
	}
	if err := bootstrap.CheckReady(vm, secret, false); err != nil {
		return nil, err
	}
	return vm, nil
}
