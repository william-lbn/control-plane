package control

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
	"github.com/neondatabase/autoscaling/pkg/dsga/policy"
	"github.com/neondatabase/autoscaling/pkg/dsga/qualificationissuer"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func supplyFixture(t *testing.T, version uint32) (createPayload, dsgaCreationPlan) {
	p, plan := genericVMFixture(t, version)
	g := plan.VM.GenericGuest
	g.ProtocolVersion = 2
	g.Supply = &dsgaSupplyProfile{ProtocolVersion: 1, AgentNamespace: "kube-system", AgentServiceAccount: "autoscaler-agent", AgentDaemonSet: "autoscaler-agent", AgentDumpPort: 10300, PlatformNamespace: "neon-control-runtime", QualifierImage: "registry.invalid/qualifier@sha256:" + strings.Repeat("b", 64),
		QualifierServiceAccount: "qualification-issuer", NodeReadClusterRole: "dsga-worker-nodes", ResourceReadHostDirectory: "/run/neon-dsga", ResourceReadSocketName: "resource.sock",
		HostLeases: []dsgaSupplyLease{{"kube-system", "host-117"}, {"kube-system", "host-118"}, {"kube-system", "host-119"}},
		Targets:    []qualificationissuer.QualifiedIntent{{VMMemoryBytes: dsgapi.Uint64(p.MinMem) << 20, NormalMemoryMaxBytes: 128 << 20}, {VMMemoryBytes: dsgapi.Uint64(p.MaxMem) << 20, NormalMemoryMaxBytes: 256 << 20}},
		Automatic: policy.Config{Targets: []policy.Target{{VMMemoryBytes: dsgapi.Uint64(p.MinMem) << 20, Buffers: uint32(plan.Budget.BootstrapBufferBytes / 8192)}, {VMMemoryBytes: dsgapi.Uint64(p.MaxMem) << 20, Buffers: uint32(plan.Budget.MaximumBufferBytes / 8192)}},
			SampleIntervalSeconds: 2, MaximumSampleGapSeconds: 10, GrowWindowSeconds: 4, ShrinkWindowSeconds: 10, CooldownSeconds: 4, RequestDeadlineSeconds: 120, ShrinkThresholdPermille: 200, GrowThresholdPermille: 700, EmergencyThresholdPermille: 900}}
	return p, plan
}
func TestDSGASupplyCommonReleaseIsFrozenAndIdentitySpecific(t *testing.T) {
	for _, version := range []uint32{17, 18} {
		p, plan := supplyFixture(t, version)
		vm, err := renderDSGAVM(p, plan)
		if err != nil {
			t.Fatal(err)
		}
		if vm.Spec.ServiceAccountName != vm.Name+"-runner" {
			t.Fatal("shared test Runner identity")
		}
		vm.UID = "original-vm"
		vm.ResourceVersion = "1"
		vm.Status.Node = "worker-119"
		frozen := plan.VM.clone()
		plan.VM.GenericGuest.Supply.Automatic.Targets[0].Buffers++
		if reflect.DeepEqual(frozen.GenericGuest.Supply.Automatic, plan.VM.GenericGuest.Supply.Automatic) {
			t.Fatal("policy aliases release")
		}
		plan.VM = frozen
		cm, job, err := dsgaQualificationJob(vm, plan, "original-node", 17)
		if err != nil {
			t.Fatal(err)
		}
		var config qualificationissuer.CollectorConfig
		if json.Unmarshal([]byte(cm.Data["collector.json"]), &config) != nil || config.Binding.VMUID != vm.UID || config.NodeName != "worker-119" || config.PreviousQualification != nil {
			t.Fatal("foreign qualification")
		}
		if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || job.Spec.Template.Spec.RestartPolicy != core.RestartPolicyNever || job.Spec.Template.Spec.HostPID || job.Spec.Template.Spec.Containers[0].Image != plan.VM.supply().QualifierImage {
			t.Fatal("unsafe Job execution")
		}
		if job.Spec.Template.Spec.Containers[0].Args[0] != "--collect-publish-config" {
			t.Fatal("shell-dependent supply")
		}
		job.UID = "original-job"
		job.ResourceVersion = "1"
		if !sameDSGAQualificationJob(job, job.DeepCopy()) {
			t.Fatal("original Job refused")
		}
		for _, kind := range []string{"image", "node", "retry", "privileged", "extra-volume", "extra-container", "script", "env", "deleted"} {
			t.Run(kind, func(t *testing.T) {
				changed := job.DeepCopy()
				spec := &changed.Spec.Template.Spec
				switch kind {
				case "image":
					spec.Containers[0].Image = "other"
				case "node":
					spec.NodeName = "master"
				case "retry":
					*changed.Spec.BackoffLimit = 1
				case "privileged":
					yes := true
					spec.Containers[0].SecurityContext.Privileged = &yes
				case "extra-volume":
					spec.Volumes = append(spec.Volumes, core.Volume{Name: "untrusted"})
				case "extra-container":
					spec.Containers = append(spec.Containers, core.Container{Name: "untrusted"})
				case "script":
					spec.Containers[0].Command = []string{"/bin/sh"}
				case "env":
					spec.Containers[0].Env = []core.EnvVar{{Name: "UNTRUSTED", Value: "value"}}
				case "deleted":
					now := metav1.Now()
					changed.DeletionTimestamp = &now
				}
				if sameDSGAQualificationJob(changed, job) {
					t.Fatal("altered qualification job accepted")
				}
			})
		}
		objects := dsgaSupplyAccess(vm, plan)
		var runnerRole, qualifierRole *rbac.Role
		for _, obj := range objects {
			if role, ok := obj.(*rbac.Role); ok {
				if role.Name == dsgaRunnerAccount(vm.Name) {
					runnerRole = role
				}
				if role.Name == dsgaRunnerAccount(vm.Name)+"-qualifier" {
					qualifierRole = role
				}
				for _, rule := range role.Rules {
					for _, v := range rule.Verbs {
						if v == "delete" || v == "patch" || v == "*" {
							t.Fatal("excessive write authority")
						}
					}
				}
			}
		}
		if runnerRole == nil || qualifierRole == nil {
			t.Fatal("missing per instance access")
		}
		for _, role := range []*rbac.Role{runnerRole, qualifierRole} {
			for _, rule := range role.Rules {
				if reflect.DeepEqual(rule.Resources, []string{"secrets"}) && !reflect.DeepEqual(rule.ResourceNames, []string{vm.Name + "-config"}) {
					t.Fatal("business credential exposed")
				}
				if reflect.DeepEqual(rule.Verbs, []string{"update"}) && reflect.DeepEqual(rule.Resources, []string{"configmaps"}) && (role == qualifierRole || len(rule.ResourceNames) != 1 || !strings.HasPrefix(rule.ResourceNames[0], "dsga-s-")) {
					t.Fatal("qualification can mutate resources")
				}
			}
		}
	}
}
func TestDSGASupplyRejectsIncompleteAndOutOfClassRelease(t *testing.T) {
	for _, kind := range []string{"namespace", "image", "host", "targets", "policy", "slot", "maxbuffers", "class", "version"} {
		t.Run(kind, func(t *testing.T) {
			p, plan := supplyFixture(t, 17)
			s := plan.VM.supply()
			switch kind {
			case "namespace":
				s.PlatformNamespace = plan.Namespace
			case "image":
				s.QualifierImage = "image:latest"
			case "host":
				s.HostLeases[1] = s.HostLeases[0]
			case "targets":
				s.Targets[0].NormalMemoryMaxBytes = 0
			case "policy":
				s.Automatic.RequestDeadlineSeconds = 0
			case "slot":
				s.Targets[0].VMMemoryBytes++
			case "maxbuffers":
				s.Automatic.Targets[1].Buffers++
			case "class":
				p.MaxMem += 1024
			case "version":
				plan.VM.GenericGuest.ProtocolVersion = 1
			}
			if plan.validate(p) == nil {
				t.Fatal("invalid automatic supply accepted")
			}
		})
	}
}
func TestDSGASupplyFactsKeepOriginalUIDAndRequireCurrentLease(t *testing.T) {
	ctx, b, _, _, _, _, _, fact := initializationBindingFixture(t, "create_project")
	// fixture has original operation, VM and immutable Config, no real cluster effects.
	v := dsgaSupplyObject{"job", "original-job", "hash"}
	if _, err := b.supplyFact(ctx, "qualification_job", v); err != nil {
		t.Fatal(err)
	}
	if _, err := b.supplyFact(ctx, "qualification_job", v); err != nil {
		t.Fatal(err)
	}
	v.UID = "replacement"
	if _, err := b.supplyFact(ctx, "qualification_job", v); err == nil {
		t.Fatal("replaced original job accepted")
	}
	if _, err := b.server.db.Exec(ctx, "UPDATE dsga_creation_supply_facts SET value='{}'"); err == nil {
		t.Fatal("immutable receipt editable")
	}
	if _, err := b.server.db.Exec(ctx, "UPDATE operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1", b.operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.supplyFact(context.Background(), "qualification_result", fact); err == nil {
		t.Fatal("expired worker published fact")
	}
}
