package control

import (
	"testing"

	dsgapi "github.com/neondatabase/autoscaling/pkg/api/dsga"
)

func TestDSGAUnopenedBootRequiresInitialClosedGate(t *testing.T) {
	instance := dsgapi.Instance{VMUID: "original-vm", ComputeInstanceID: "original-compute", PGBootID: 4}
	original := dsgapi.Snapshot{Instance: instance, CapacityGeneration: 1}
	if !isDSGAUnopenedBoot(original, instance) {
		t.Fatal("current boot with its untouched closed gate was rejected")
	}
	cases := map[string]func(*dsgapi.Snapshot){
		"different-boot": func(s *dsgapi.Snapshot) { s.Instance.PGBootID++ },
		"open":           func(s *dsgapi.Snapshot) { s.AdmissionOpen = true },
		"prior-admission": func(s *dsgapi.Snapshot) {
			s.AdmissionRevision = 1
		},
		"issuer-changed":   func(s *dsgapi.Snapshot) { s.IssuerFloorEpoch = 1 },
		"capacity-changed": func(s *dsgapi.Snapshot) { s.CapacityGeneration++ },
		"native-command":   func(s *dsgapi.Snapshot) { s.Operation = &dsgapi.OperationFact{} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			changed := original
			change(&changed)
			if isDSGAUnopenedBoot(changed, instance) {
				t.Fatal("non-initial PG state was accepted for retirement")
			}
		})
	}
}
