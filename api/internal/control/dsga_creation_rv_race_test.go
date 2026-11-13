package control

import (
	"context"
	"reflect"
	"testing"
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
