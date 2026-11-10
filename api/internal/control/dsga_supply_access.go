package control

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"github.com/neondatabase/autoscaling/pkg/dsga/journal"
	"github.com/neondatabase/autoscaling/pkg/dsga/vmguard"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const dsgaSupplyVMLabel = "internal.neon-control/supply-vm-uid"

// 每VM只读取自己的启动Secret和Owner租约。CM创建因Kubernetes语义不能限制
// resourceNames；可变state的update仍限定原协议导出的单一对象。平台策略在另一namespace。
func dsgaSupplyAccess(vm *vmv1.VirtualMachine, p dsgaCreationPlan) []any {
	s := p.VM.supply()
	if s == nil || vm == nil || vm.UID == "" {
		return nil
	}
	b := journal.Binding{Namespace: vm.Namespace, Name: vm.Name, VMUID: vm.UID, Incarnation: p.Incarnation}
	account := dsgaRunnerAccount(vm.Name)
	metadata := func(name, ns string) metav1.ObjectMeta {
		m := metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{dsgaSupplyVMLabel: string(vm.UID)}}
		if ns == vm.Namespace {
			yes, no := true, false
			m.OwnerReferences = []metav1.OwnerReference{{APIVersion: vmv1.SchemeGroupVersion.String(), Kind: "VirtualMachine", Name: vm.Name, UID: vm.UID, Controller: &yes, BlockOwnerDeletion: &no}}
		}
		return m
	}
	subject := []rbac.Subject{{Kind: "ServiceAccount", Namespace: vm.Namespace, Name: account}}
	no := false
	sa := &core.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: metadata(account, vm.Namespace), AutomountServiceAccountToken: &no}
	// BindingName/stateName共用原v1摘要；仅前缀区分两种原来源，不根据Pod/Owner另造账本。
	stateName := "dsga-s-" + strings.TrimPrefix(vmguard.BindingName(b), "dsga-b-")
	rules := []rbac.PolicyRule{
		{APIGroups: []string{"vm.neon.tech"}, Resources: []string{"virtualmachines/finalizers"}, ResourceNames: []string{vm.Name}, Verbs: []string{"update"}},
		{APIGroups: []string{"vm.neon.tech"}, Resources: []string{"virtualmachines"}, ResourceNames: []string{vm.Name}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{vm.Name + "-config"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "create"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{stateName}, Verbs: []string{"update"}},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, ResourceNames: []string{journal.LeaseName(b)}, Verbs: []string{"get"}},
	}
	role := &rbac.Role{TypeMeta: metav1.TypeMeta{APIVersion: rbac.SchemeGroupVersion.String(), Kind: "Role"}, ObjectMeta: metadata(account, vm.Namespace), Rules: rules}
	binding := &rbac.RoleBinding{TypeMeta: role.TypeMeta, ObjectMeta: metadata(account, vm.Namespace), RoleRef: rbac.RoleRef{APIGroup: rbac.GroupName, Kind: "Role", Name: account}, Subjects: subject}
	binding.Kind = "RoleBinding"
	// ClusterRole本身由Helm只读固定117/118/119；这里不允许按租户输入扩大规则。
	cluster := &rbac.ClusterRoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbac.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding"}, ObjectMeta: metadata(vm.Namespace+"-"+account, ""), RoleRef: rbac.RoleRef{APIGroup: rbac.GroupName, Kind: "ClusterRole", Name: s.NodeReadClusterRole}, Subjects: subject}
	out := []any{sa, role, binding, cluster}
	for i, l := range s.HostLeases {
		name := vm.Namespace + "-" + account + "-host-" + string(rune('0'+i))
		hr := &rbac.Role{TypeMeta: role.TypeMeta, ObjectMeta: metadata(name, l.Namespace), Rules: []rbac.PolicyRule{{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, ResourceNames: []string{l.Name}, Verbs: []string{"get"}}}}
		hb := &rbac.RoleBinding{TypeMeta: binding.TypeMeta, ObjectMeta: metadata(name, l.Namespace), RoleRef: rbac.RoleRef{APIGroup: rbac.GroupName, Kind: "Role", Name: name}, Subjects: subject}
		out = append(out, hr, hb)
	}
	// 独立Qualifier身份只获该VM的读取和原资格CM发布权限；不获VM/Node/PG写权限。
	qr := role.DeepCopy()
	qr.Name = account + "-qualifier"
	qr.Rules = nil
	for _, rule := range rules {
		if len(rule.Verbs) == 1 && rule.Verbs[0] == "update" && rule.Resources[0] == "configmaps" {
			continue
		}
		qr.Rules = append(qr.Rules, rule)
	}
	qb := binding.DeepCopy()
	qb.Name = qr.Name
	qb.RoleRef.Name = qr.Name
	qb.Subjects = []rbac.Subject{{Kind: "ServiceAccount", Namespace: s.PlatformNamespace, Name: s.QualifierServiceAccount}}
	return append(out, qr, qb)
}

// 权限对象可幂等恢复，但绝不覆盖同名的外来/扩大权限对象；未知CREATE留待原操作读回。
// 与资源资格分开：这些对象只授予调用权，不作为内存/启动成功证明。
func (k *kubeClient) ensureDSGASupplyAccess(ctx context.Context, vm *vmv1.VirtualMachine, p dsgaCreationPlan) error {
	for _, object := range dsgaSupplyAccess(vm, p) {
		raw, err := json.Marshal(object)
		if err != nil {
			return err
		}
		var wanted map[string]any
		_ = json.Unmarshal(raw, &wanted)
		meta := wanted["metadata"].(map[string]any)
		name := meta["name"].(string)
		ns, _ := meta["namespace"].(string)
		kind := wanted["kind"].(string)
		resource, root := "", "/apis/rbac.authorization.k8s.io/v1"
		switch kind {
		case "ServiceAccount":
			resource = "serviceaccounts"
			root = "/api/v1"
		case "Role":
			resource = "roles"
		case "RoleBinding":
			resource = "rolebindings"
		case "ClusterRoleBinding":
			resource = "clusterrolebindings"
		default:
			return errDSGACreationPlan
		}
		path := root
		if ns != "" {
			path += "/namespaces/" + ns
		}
		path += "/" + resource
		got, err := k.request(ctx, http.MethodGet, path+"/"+name, nil)
		if kubeStatusIs(err, 404) {
			got, err = k.request(ctx, http.MethodPost, path, wanted)
		}
		if err != nil {
			return err
		}
		if nested(got, "metadata", "uid") == nil || nested(got, "metadata", "deletionTimestamp") != nil ||
			nested(got, "metadata", "labels", dsgaSupplyVMLabel) != string(vm.UID) ||
			!reflect.DeepEqual(nested(got, "metadata", "ownerReferences"), meta["ownerReferences"]) {
			return errDSGACreationIdentity
		}
		for _, field := range []string{"rules", "roleRef", "subjects", "automountServiceAccountToken"} {
			if !reflect.DeepEqual(got[field], wanted[field]) {
				return errDSGACreationIdentity
			}
		}
	}
	return nil
}
