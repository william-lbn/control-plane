package control

import (
	"bytes"
	"encoding/json"

	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/resourceguard"
)

// v4冻结通用Guest的公开构建策略；v5在该Guest合同上增加Runner Pod硬预算。
// 它是发行意图，不是资格报告或运行时证明；
// 真实原SecretUID/Runtime摘要仍由原Controller/Runner填写和核验。
type dsgaGenericGuestProfile struct {
	ProtocolVersion uint32             `json:"protocol_version"`
	PolicyTemplates json.RawMessage    `json:"policy_templates"`
	Supply          *dsgaSupplyProfile `json:"supply,omitempty"`
}
type dsgaGuestPolicyTemplate struct {
	Asset  string          `json:"asset"`
	Policy json.RawMessage `json:"policy"`
}

func (p *dsgaGenericGuestProfile) templates() ([]dsgaGuestPolicyTemplate, error) {
	if p == nil || (p.ProtocolVersion != 1 && p.ProtocolVersion != 2) || len(p.PolicyTemplates) > 16<<10 || dsgapi.ValidateJSON(p.PolicyTemplates, 16<<10, 8192) != nil {
		return nil, errDSGACreationPlan
	}
	if p.ProtocolVersion == 1 && p.Supply != nil || p.ProtocolVersion == 2 && (p.Supply == nil || p.Supply.validate() != nil) {
		return nil, errDSGACreationPlan
	}
	var envelope struct {
		ProtocolVersion uint32                    `json:"protocol_version"`
		Scope           string                    `json:"scope"`
		Templates       []dsgaGuestPolicyTemplate `json:"templates"`
	}
	if dsgapi.UnmarshalObject(p.PolicyTemplates, &envelope) != nil || envelope.ProtocolVersion != 1 || envelope.Scope != "neon-dsga-policy-templates-v1" || len(envelope.Templates) != 5 {
		return nil, errDSGACreationPlan
	}
	expected := map[string]bool{"dsga-physical-kernel.json": true, "dsga-kernel-bootstrap.json": true, "dsga-prebootstrap-system.json": true, "dsga-normal-split.json": true, "dsga-native-birth.json": true}
	for _, entry := range envelope.Templates {
		if !expected[entry.Asset] || len(entry.Policy) == 0 || entry.Policy[0] != '{' {
			return nil, errDSGACreationPlan
		}
		delete(expected, entry.Asset)
	}
	return envelope.Templates, nil
}
func (p *dsgaGenericGuestProfile) validateNative(version uint32, budget dsgaBootstrapBudget) error {
	entries, err := p.templates()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Asset != "dsga-native-birth.json" {
			continue
		}
		var birth struct {
			PGMajor        uint32 `json:"pg_major"`
			MaxBuffers     uint64 `json:"max_buffers"`
			StartupBuffers uint64 `json:"startup_buffers"`
		}
		if json.Unmarshal(entry.Policy, &birth) != nil || birth.PGMajor != version || birth.MaxBuffers != budget.MaximumBufferBytes/8192 || birth.StartupBuffers != budget.BootstrapBufferBytes/8192 {
			return errDSGACreationPlan
		}
	}
	return nil
}
func (p *dsgaGenericGuestProfile) clone() *dsgaGenericGuestProfile {
	if p == nil {
		return nil
	}
	out := *p
	out.PolicyTemplates = bytes.Clone(p.PolicyTemplates)
	out.Supply = p.Supply.clone()
	return &out
}
func (v dsgaVMProfile) generic() bool {
	return v.ProtocolVersion == 4 || (v.ProtocolVersion == 5 && v.GenericGuest != nil)
}
func genericStartupSelection() []byte {
	raw, _ := json.Marshal(resourceguard.ComputeStartupSelection{ProtocolVersion: 1, IdentitySource: resourceguard.RuntimeComputeIdentitySource})
	return raw
}
