package control

import (
	"k8s.io/apimachinery/pkg/util/validation"
	"path/filepath"
	"slices"
	"strings"

	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/policy"
	"github.com/neondatabase/autoscaling/pkg/dsga/qualificationissuer"
)

// v2发行冻结供应所需的可信部署输入。这里没有资格事实、JWT或客户可指定的命令。
// Collector/Issuer仍从原节点/Root独立取证；Owner仍是唯一内存执行者。
type dsgaSupplyProfile struct {
	AgentNamespace            string                                `json:"agent_namespace"`
	AgentServiceAccount       string                                `json:"agent_service_account"`
	AgentDaemonSet            string                                `json:"agent_daemon_set"`
	AgentDumpPort             uint16                                `json:"agent_dump_port"`
	ProtocolVersion           uint32                                `json:"protocol_version"`
	PlatformNamespace         string                                `json:"platform_namespace"`
	QualifierImage            string                                `json:"qualifier_image"`
	QualifierServiceAccount   string                                `json:"qualifier_service_account"`
	AdapterServiceAccount     string                                `json:"adapter_service_account,omitempty"`
	NodeReadClusterRole       string                                `json:"node_read_cluster_role"`
	ResourceReadHostDirectory string                                `json:"resource_read_host_directory"`
	ResourceReadSocketName    string                                `json:"resource_read_socket_name"`
	HostLeases                []dsgaSupplyLease                     `json:"host_leases"`
	Targets                   []qualificationissuer.QualifiedIntent `json:"targets"`
	Automatic                 policy.Config                         `json:"automatic"`
}
type dsgaSupplyLease struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (p *dsgaSupplyProfile) validate() error {
	if p == nil || p.ProtocolVersion != 1 || !dsgaPlanNamespace.MatchString(p.AgentNamespace) || !dsgaPlanNamespace.MatchString(p.AgentServiceAccount) || !dsgaPlanNamespace.MatchString(p.AgentDaemonSet) || p.AgentDumpPort == 0 || !strings.HasPrefix(p.PlatformNamespace, "neon-control-") || len(p.PlatformNamespace) > 63 || !dsgaPlanNamespace.MatchString(p.PlatformNamespace) ||
		!dsgaPinnedImage.MatchString(p.QualifierImage) || !dsgaPlanNamespace.MatchString(p.QualifierServiceAccount) || len(p.QualifierServiceAccount) > 63 ||
		(p.AdapterServiceAccount != "" && p.AdapterServiceAccount != "neon-control-adapter") ||
		dsgapi.ValidateID(p.NodeReadClusterRole, "node read role") != nil || !filepath.IsAbs(p.ResourceReadHostDirectory) || filepath.Clean(p.ResourceReadHostDirectory) != p.ResourceReadHostDirectory ||
		p.ResourceReadHostDirectory == "/" || len(validation.IsDNS1123Subdomain(p.ResourceReadSocketName)) != 0 || len(p.HostLeases) != 3 || len(p.Targets) < 1 || len(p.Targets) > 32 || p.Automatic.Validate() != nil {
		return errDSGACreationPlan
	}
	seen := map[dsgaSupplyLease]bool{}
	for _, l := range p.HostLeases {
		if !dsgaPlanNamespace.MatchString(l.Namespace) || dsgapi.ValidateID(l.Name, "host lease") != nil || seen[l] {
			return errDSGACreationPlan
		}
		seen[l] = true
	}
	var prev dsgapi.Uint64
	for _, t := range p.Targets {
		if t.VMMemoryBytes <= prev || t.NormalMemoryMaxBytes == 0 {
			return errDSGACreationPlan
		}
		prev = t.VMMemoryBytes
	}
	for _, t := range p.Automatic.Targets {
		if !slices.ContainsFunc(p.Targets, func(q qualificationissuer.QualifiedIntent) bool { return q.VMMemoryBytes == t.VMMemoryBytes }) {
			return errDSGACreationPlan
		}
	}
	return nil
}
func (p *dsgaSupplyProfile) clone() *dsgaSupplyProfile {
	if p == nil {
		return nil
	}
	v := *p
	v.HostLeases = slices.Clone(p.HostLeases)
	v.Targets = slices.Clone(p.Targets)
	v.Automatic.Targets = slices.Clone(p.Automatic.Targets)
	return &v
}
func (v dsgaVMProfile) supply() *dsgaSupplyProfile {
	if !v.generic() || v.GenericGuest == nil || v.GenericGuest.ProtocolVersion != 2 {
		return nil
	}
	return v.GenericGuest.Supply
}
func (p dsgaCreationPlan) validateSupply(input createPayload) error {
	s := p.VM.supply()
	if s == nil {
		return nil
	}
	if s.validate() != nil || s.PlatformNamespace == p.Namespace {
		return errDSGACreationPlan
	}
	low, high := dsgapi.Uint64(input.MinMem)*(1<<20), dsgapi.Uint64(input.MaxMem)*(1<<20)
	if s.Targets[0].VMMemoryBytes != low || s.Targets[len(s.Targets)-1].VMMemoryBytes != high {
		return errDSGAProjectBounds
	}
	for _, t := range s.Targets {
		if uint64(t.VMMemoryBytes)%p.MemorySlotBytes != 0 {
			return errDSGACreationPlan
		}
	}
	for _, t := range s.Automatic.Targets {
		if uint64(t.Buffers)*8192 > p.Budget.MaximumBufferBytes || uint64(t.Buffers)*8192 < p.Budget.BootstrapBufferBytes {
			return errDSGACreationPlan
		}
	}
	return nil
}
func dsgaRunnerAccount(name string) string { return name + "-runner" }
