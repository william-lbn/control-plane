package control

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func storageGCHistoryModel() storageGCHistorySnapshot {
	tenant, timeline := strings.Repeat("1", 32), strings.Repeat("2", 32)
	scope := storageGCHistoryScope{TenantID: tenant, TimelineID: timeline, DatabaseOID: 16384, Database: "postgres"}
	return storageGCHistorySnapshot{Inventory: storageGCHistoryInventory{Version: 1, ProjectID: "prj_0123456789abcdef", TenantID: tenant, Revision: "native-controller-generation-1", ShardCount: 1, AllShards: true, AllTimelineStates: true, NoCreatingOrImporting: true, Timelines: []storageGCHistoryTimeline{{TimelineID: timeline, Readable: true, AllDatabases: true, Scopes: []storageGCHistoryScope{scope}, Horizons: []storageGCHistoryHorizon{{Shard: 0, Complete: true, Planned: "0/2000", Applied: "0/1800", Latest: "0/4000"}}}}}, Directories: []storageGCHistoryDirectory{{Scope: scope, Incarnation: "sge_" + strings.Repeat("3", 24), SchemaSHA256: strings.Repeat("a", 64), ObservationLSN: "0/1000", Epoch: 4}}}
}

func TestStorageGCHistoryCrossSnapshotAbsenceEpochAndAllPins(t *testing.T) {
	first, current := storageGCHistoryModel(), storageGCHistoryModel()
	current.Directories[0].ObservationLSN = "0/3000"
	if got := evaluateStorageGCHistorySnapshots(first, current); got != "eligible" {
		t.Fatal(got)
	}
	for _, tc := range []struct {
		name   string
		alter  func(*storageGCHistorySnapshot)
		reason string
	}{
		{"old-lease", func(s *storageGCHistorySnapshot) { s.Inventory.Timelines[0].Horizons[0].LeasePins = []string{"0/FF0"} }, "native_retention_or_pin_held"},
		{"old-child-pin", func(s *storageGCHistorySnapshot) { s.Inventory.Timelines[0].Horizons[0].ChildPins = []string{"0/1000"} }, "native_retention_or_pin_held"},
		{"old-restore-pin", func(s *storageGCHistorySnapshot) { s.Inventory.Timelines[0].Horizons[0].RestorePins = []string{"0/10"} }, "native_retention_or_pin_held"},
		{"standby-reader", func(s *storageGCHistorySnapshot) {
			s.Inventory.Timelines[0].Horizons[0].StandbyPins = []string{"0/FF8"}
		}, "native_retention_or_pin_held"},
		{"epoch-reappeared-then-deleted", func(s *storageGCHistorySnapshot) { s.Directories[0].Epoch += 2 }, "native_reference_epoch_changed"},
		{"epoch-regressed-after-restore", func(s *storageGCHistorySnapshot) { s.Directories[0].Epoch-- }, "native_reference_epoch_changed"},
		{"object-shared-reference", func(s *storageGCHistorySnapshot) { s.Directories[0].ObjectReferences = 1 }, "native_reference_present"},
		{"aborted-or-completed-staging-part", func(s *storageGCHistorySnapshot) { s.Directories[0].MultipartReferences = 1 }, "native_reference_present"},
		{"schema-new-version-reference", func(s *storageGCHistorySnapshot) { s.Directories[0].SchemaSHA256 = strings.Repeat("b", 64) }, "native_instrumentation_changed"},
		{"reinstalled-instrumentation", func(s *storageGCHistorySnapshot) { s.Directories[0].Incarnation = "new" }, "native_instrumentation_changed"},
		{"native-topology-changed", func(s *storageGCHistorySnapshot) { s.Inventory.Revision = "native-next" }, "native_topology_changed"},
		{"attachment-shard-not-enumerated", func(s *storageGCHistorySnapshot) { s.Inventory.AllShards = false }, "current_native_inventory_incomplete"},
		{"import-in-flight", func(s *storageGCHistorySnapshot) { s.Inventory.NoCreatingOrImporting = false }, "current_native_inventory_incomplete"},
		{"offload-not-enumerated", func(s *storageGCHistorySnapshot) { s.Inventory.AllTimelineStates = false }, "current_native_inventory_incomplete"},
		{"offloaded-unreadable", func(s *storageGCHistorySnapshot) { s.Inventory.Timelines[0].Readable = false }, "current_native_inventory_incomplete"},
		{"database-not-enumerated", func(s *storageGCHistorySnapshot) { s.Inventory.Timelines[0].AllDatabases = false }, "current_native_inventory_incomplete"},
		{"unknown-native-publication", func(s *storageGCHistorySnapshot) { s.Inventory.UnknownPublications = true }, "current_native_inventory_incomplete"},
		{"pins-incomplete", func(s *storageGCHistorySnapshot) { s.Inventory.Timelines[0].Horizons[0].Complete = false }, "current_native_inventory_incomplete"},
		{"malformed-native-lsn", func(s *storageGCHistorySnapshot) { s.Inventory.Timelines[0].Horizons[0].Applied = "bad" }, "current_native_inventory_incomplete"},
		{"duplicate-native-shard", func(s *storageGCHistorySnapshot) {
			s.Inventory.ShardCount = 2
			s.Inventory.Timelines[0].Horizons = append(s.Inventory.Timelines[0].Horizons, s.Inventory.Timelines[0].Horizons[0])
		}, "current_native_inventory_incomplete"},
		{"missing-directory", func(s *storageGCHistorySnapshot) { s.Directories = nil }, "native_topology_changed"},
		{"wrong-database-oid", func(s *storageGCHistorySnapshot) { s.Directories[0].Scope.DatabaseOID++ }, "directory_missing"},
		{"retired-observation-regression", func(s *storageGCHistorySnapshot) { s.Directories[0].ObservationLSN = "0/800" }, "native_observation_lsn_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after := storageGCHistoryModel(), storageGCHistoryModel()
			after.Directories[0].ObservationLSN = "0/3000"
			tc.alter(&after)
			if reason := evaluateStorageGCHistorySnapshots(before, after); reason != tc.reason {
				t.Fatalf("got %s want %s", reason, tc.reason)
			}
		})
	}
	// The public min_readable_lsn cannot stand in for the full pin list. A
	// numerically large planned/applied boundary still retains a lower lease.
	h := storageGCHistoryHorizon{Complete: true, Planned: "1/0", Applied: "0/FFFF0000", Latest: "2/0", LeasePins: []string{"0/FF8"}}
	if floor, e := storageGCHistoryEffectiveFloor(h); e != nil || floor != 0xff8 {
		t.Fatal("minimum live lease was lost", floor, e)
	}
	before, after := storageGCHistoryModel(), storageGCHistoryModel()
	for _, s := range []*storageGCHistorySnapshot{&before, &after} {
		s.Inventory.ShardCount = 2
		second := s.Inventory.Timelines[0].Horizons[0]
		second.Shard = 1
		s.Inventory.Timelines[0].Horizons = append(s.Inventory.Timelines[0].Horizons, second)
	}
	after.Directories[0].ObservationLSN = "0/3000"
	if reason := evaluateStorageGCHistorySnapshots(before, after); reason != "eligible" {
		t.Fatal("complete second shard rejected", reason)
	}
	after.Inventory.Timelines[0].Horizons[1].LeasePins = []string{"0/FF8"}
	if reason := evaluateStorageGCHistorySnapshots(before, after); reason != "native_retention_or_pin_held" {
		t.Fatal("nonzero shard lease was ignored", reason)
	}
}

type storageGCHistoryFakeNative struct {
	snapshot                  storageGCHistorySnapshot
	active                    bool
	checks, retirements       int
	failCheck, failRetirement bool
}

func (v *storageGCHistoryFakeNative) Inventory() storageGCHistoryInventory {
	return v.snapshot.Inventory
}
func (v *storageGCHistoryFakeNative) Check(context.Context) error {
	v.checks++
	if !v.active || v.failCheck {
		return errStorageGCNativeProof
	}
	return nil
}
func (v *storageGCHistoryFakeNative) RegisterRetirement(context.Context, string, string, []storageGCHistoryScope) error {
	if !v.active || v.failRetirement {
		return errStorageGCNativeProof
	}
	v.retirements++
	return nil
}
func (v *storageGCHistoryFakeNative) WithFence(_ context.Context, _ string, _ string, fn func(storageGCHistoryNativeLease) error) error {
	v.active = true
	defer func() { v.active = false }()
	return fn(v)
}

type storageGCHistoryFakeSource struct {
	native                      *storageGCHistoryFakeNative
	opened, closed, quarantined int
	failOpen, failQuarantine    bool
	failAfter                   int
}
type storageGCHistoryFakeLease struct {
	source *storageGCHistoryFakeSource
	value  storageGCHistoryDirectory
	closed bool
}

func (v *storageGCHistoryFakeSource) Open(_ context.Context, scope storageGCHistoryScope, _, _ string) (storageGCHistoryDirectoryLease, error) {
	if v.failOpen || !v.native.active || v.failAfter > 0 && v.opened >= v.failAfter {
		return nil, errStorageGCNativeProof
	}
	for _, d := range v.native.snapshot.Directories {
		if d.Scope == scope {
			v.opened++
			return &storageGCHistoryFakeLease{source: v, value: d}, nil
		}
	}
	return nil, errStorageGCNativeProof
}
func (v *storageGCHistoryFakeLease) Observation() storageGCHistoryDirectory { return v.value }
func (v *storageGCHistoryFakeLease) Check(context.Context) error {
	if v.closed || !v.source.native.active {
		return errStorageGCNativeProof
	}
	return nil
}
func (v *storageGCHistoryFakeLease) Quarantine(context.Context, string) error {
	if v.closed || v.source.failQuarantine {
		return errStorageGCNativeProof
	}
	v.source.quarantined++
	return nil
}
func (v *storageGCHistoryFakeLease) Close(context.Context) error {
	if !v.closed {
		v.closed = true
		v.source.closed++
	}
	return nil
}

func TestStorageGCHistoryRequiresActualScopeLeasesAndUnavailableNativeHolds(t *testing.T) {
	native := &storageGCHistoryFakeNative{snapshot: storageGCHistoryModel(), active: true}
	source := &storageGCHistoryFakeSource{native: native}
	_, leases, err := captureStorageGCHistorySnapshot(context.Background(), native.Inventory(), source, native.snapshot.Inventory.ProjectID, native.snapshot.Inventory.ProjectID+"/blob_"+strings.Repeat("a", 24))
	if err != nil || source.opened != 1 {
		t.Fatal(err)
	}
	if err = leases[0].Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	closeStorageGCHistoryLeases(leases)
	if source.closed != 1 || leases[0].Check(context.Background()) == nil {
		t.Fatal("closed scope lease stayed valid")
	}
	second := native.snapshot.Directories[0]
	second.Scope.DatabaseOID++
	second.Scope.Database = "postgres2"
	native.snapshot.Directories = append(native.snapshot.Directories, second)
	native.snapshot.Inventory.Timelines[0].Scopes = append(native.snapshot.Inventory.Timelines[0].Scopes, second.Scope)
	partial := &storageGCHistoryFakeSource{native: native, failAfter: 1}
	if _, _, e := captureStorageGCHistorySnapshot(context.Background(), native.Inventory(), partial, native.snapshot.Inventory.ProjectID, native.snapshot.Inventory.ProjectID+"/blob_"+strings.Repeat("a", 24)); e == nil || partial.opened != 1 || partial.closed != 1 {
		t.Fatal("partial scope failure leaked a live native lease", e, partial.opened, partial.closed)
	}
	called := false
	err = (unavailableStorageGCHistoryAuthority{}).WithFence(context.Background(), "project", "tenant", func(storageGCHistoryNativeLease) error { called = true; return nil })
	if !errors.Is(err, errStorageGCNativeProof) || called {
		t.Fatal("missing upstream native API was replaced with a fabricated proof")
	}
}
