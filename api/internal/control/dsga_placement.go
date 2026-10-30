package control

import (
	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sort"
)

const dsgaMemoryPoolLabel = "neon.tech/pg17-memory-pool"

// dsgaVMPlacement只承载受信平台在接受前明确选择的节点范围，不接租户请求。
// 完整selector和hostname名单进入原plan；恢复不读env、不查同名节点收养授权。
// 类型置于指针下，避免map/slice破坏dsgaVMProfile及plan既有零值比较合同。
type dsgaVMPlacement struct {
	NodeSelector     map[string]string `json:"node_selector"`
	AllowedNodeNames []string          `json:"allowed_node_names"`
}

func (p *dsgaVMPlacement) validate(architecture string) error {
	if p == nil || len(p.NodeSelector) < 3 || len(p.NodeSelector) > 16 ||
		p.NodeSelector[core.LabelOSStable] != "linux" || p.NodeSelector[core.LabelArchStable] != architecture ||
		p.NodeSelector[dsgaMemoryPoolLabel] == "" || len(p.AllowedNodeNames) != 3 {
		return errDSGACreationPlan
	}
	for key, value := range p.NodeSelector {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
			return errDSGACreationPlan
		}
	}
	seen := make(map[string]bool, 3)
	for _, name := range p.AllowedNodeNames {
		if len(validation.IsDNS1123Subdomain(name)) != 0 || seen[name] {
			return errDSGACreationPlan
		}
		seen[name] = true
	}
	return nil
}

func (p *dsgaVMPlacement) clone() *dsgaVMPlacement {
	if p == nil {
		return nil
	}
	copy := *p
	copy.NodeSelector = make(map[string]string, len(p.NodeSelector))
	for key, value := range p.NodeSelector {
		copy.NodeSelector[key] = value
	}
	copy.AllowedNodeNames = append([]string(nil), p.AllowedNodeNames...)
	return &copy
}

// apply只投影已验证的原plan。单一required term的hostname与完整NodeSelector
// 是AND，不能另加一个仅arch的OR term放宽名单。输出独占map/slice，API对象
// 的后续变更不得反向修改原计划；排序仅作用于副本，不重写已持久接受的字节。
func (p *dsgaVMPlacement) apply(vm *vmv1.VirtualMachine) {
	copy := p.clone()
	sort.Strings(copy.AllowedNodeNames)
	vm.Spec.NodeSelector = copy.NodeSelector
	vm.Spec.Affinity = &core.Affinity{NodeAffinity: &core.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &core.NodeSelector{NodeSelectorTerms: []core.NodeSelectorTerm{{
			MatchExpressions: []core.NodeSelectorRequirement{{Key: core.LabelHostname, Operator: core.NodeSelectorOpIn, Values: copy.AllowedNodeNames}},
		}}},
	}}
}
