package control

import (
	"fmt"
	"testing"
	"time"
)

func TestDSGAResumePlanPreservesLogicalIdentityAndRefreshesBootInputs(t *testing.T) {
	for _, major := range []uint32{17, 18} {
		t.Run(fmt.Sprint(major), func(t *testing.T) {
			p, previous := supplyFixture(t, major)
			policy := dsgaCreationPolicy{
				PostgresVersion: major, NativeBootstrapVersion: previous.NativeBootstrapVersion,
				Namespace: previous.Namespace, Image: previous.Image, MemorySlotBytes: previous.MemorySlotBytes,
				Budget: previous.Budget, Storage: previous.Storage, VM: previous.VM.clone(), RetainedCatalog: true,
			}
			next, err := newDSGAResumePlan(p, previous, policy, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if next.Credentials != previous.Credentials || next.ComputeInstanceID != previous.ComputeInstanceID ||
				next.PostgresVersion != major || next.Storage != previous.Storage || !next.RetainCatalog {
				t.Fatal("resume changed the original logical Compute or credential source")
			}
			if next.Incarnation == previous.Incarnation || next.OperationUUID == previous.OperationUUID {
				t.Fatal("resume reused the previous physical generation")
			}
			for _, changed := range []dsgaCreationPolicy{
				{PostgresVersion: 16, RetainedCatalog: true},
				{PostgresVersion: major, RetainedCatalog: false},
			} {
				rejected := policy
				rejected.PostgresVersion, rejected.RetainedCatalog = changed.PostgresVersion, changed.RetainedCatalog
				if _, err := newDSGAResumePlan(p, previous, rejected, time.Now()); err == nil {
					t.Fatal("resume accepted incompatible release")
				}
			}
		})
	}
}
