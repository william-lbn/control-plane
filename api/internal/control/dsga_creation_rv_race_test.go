package control

import (
	"context"
	"reflect"
	"testing"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

func TestDSGACreationReReadsRVAfterNeonVMFinalizer(t *testing.T) {
	f, input := newCreationTest(t)
	f.afterCreate = func() {
		// Simulate NeonVM Controller adding its finalizer after the CREATE
		// response was formed but before the control plane initializes the journal.
		f.vm.Finalizers = append(f.vm.Finalizers, "vm.neon.tech/finalizer")
		f.vm.ResourceVersion = f.next()
	}
	vm, err := f.kube.prepareDSGACreation(context.Background(), input)
	if err != nil {
		t.Fatalf("creation should re-read the same held VM and initialize with its fresh RV: %v", err)
	}
	if vm == nil || !vm.Spec.DSGABootstrap.Ready {
		t.Fatal("bootstrap was not released after same-UID journal/config confirmation")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	want := []string{"create", "bind", "initialize", "config", "bind-config", "release"}
	if !reflect.DeepEqual(f.trace, want) {
		t.Fatalf("creation side-effect order changed: got %v want %v", f.trace, want)
	}
	if len(f.vm.Finalizers) != 1 || f.vm.Finalizers[0] != "vm.neon.tech/finalizer" {
		t.Fatalf("NeonVM controller finalizer was lost: %v", f.vm.Finalizers)
	}
}

func TestDSGACreationRefreshesHeldSourceAfterAccessPreparation(t *testing.T) {
	for _, changed := range []string{"metadata", "spec", "config"} {
		t.Run(changed, func(t *testing.T) {
			f, input := newCreationTest(t)
			input.BeforeRelease = func(_ context.Context, _ *vmv1.VirtualMachine) error {
				f.mu.Lock()
				defer f.mu.Unlock()
				switch changed {
				case "metadata":
					f.vm.Finalizers = append(f.vm.Finalizers, "vm.neon.tech/finalizer")
				case "spec":
					f.vm.Spec.ServiceAccountName = "changed-after-rbac"
				case "config":
					f.secret.UID = "changed-after-rbac"
				}
				f.vm.ResourceVersion = f.next()
				return nil
			}
			vm, err := f.kube.prepareDSGACreation(context.Background(), input)
			f.mu.Lock()
			defer f.mu.Unlock()
			want := []string{"create", "bind", "initialize", "config", "bind-config"}
			if changed == "metadata" {
				want = append(want, "release")
				if err != nil || vm == nil || !vm.Spec.DSGABootstrap.Ready || len(vm.Finalizers) != 1 {
					t.Fatalf("same held source could not release after metadata changed: %v", err)
				}
			} else if err == nil || vm != nil || f.vm.Spec.DSGABootstrap.Ready {
				t.Fatalf("changed %s source released bootstrap: %v", changed, err)
			}
			if !reflect.DeepEqual(f.trace, want) {
				t.Fatalf("unexpected writes: got %v want %v", f.trace, want)
			}
		})
	}
}
