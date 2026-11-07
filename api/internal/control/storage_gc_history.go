package control

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// These inputs are supplied ONLY by a native authority holding a continuous
// tenant/shard fence, never an HTTP request, metadata branch list or timestamp.
// The authority must include creating/importing/offloaded/detached timelines,
// every database, all lease/child/restore/standby pins and attachment generations.
type storageGCHistoryInventory struct {
	Version               int                        `json:"version"`
	ProjectID             string                     `json:"project_id"`
	TenantID              string                     `json:"tenant_id"`
	Revision              string                     `json:"revision"`
	ShardCount            int                        `json:"shard_count"`
	AllShards             bool                       `json:"all_shards"`
	AllTimelineStates     bool                       `json:"all_timeline_states"`
	NoCreatingOrImporting bool                       `json:"no_creating_or_importing"`
	UnknownPublications   bool                       `json:"unknown_publications"`
	Timelines             []storageGCHistoryTimeline `json:"timelines"`
}
type storageGCHistoryTimeline struct {
	TimelineID   string                    `json:"timeline_id"`
	AncestorID   string                    `json:"ancestor_id"`
	Readable     bool                      `json:"readable"`
	AllDatabases bool                      `json:"all_databases"`
	Scopes       []storageGCHistoryScope   `json:"scopes"`
	Horizons     []storageGCHistoryHorizon `json:"horizons"`
}
type storageGCHistoryHorizon struct {
	Shard    int    `json:"shard"`
	Complete bool   `json:"complete"`
	Planned  string `json:"planned_lsn"`
	Applied  string `json:"applied_lsn"`
	Latest   string `json:"latest_lsn"`
	// Even expired-looking pins must remain until the native authority proves
	// expiry/absence under its own clock and GC synchronization. We never prune.
	ChildPins   []string `json:"child_pins"`
	LeasePins   []string `json:"lease_pins"`
	RestorePins []string `json:"restore_pins"`
	StandbyPins []string `json:"standby_pins"`
}
type storageGCHistorySnapshot struct {
	Inventory   storageGCHistoryInventory   `json:"inventory"`
	Directories []storageGCHistoryDirectory `json:"directories"`
}

// A callback runs while the native authority still HOLDS the fence, including
// across all SQL reads and physical DELETE. A time-limited snapshot token or a
// FenceHeld JSON flag is explicitly insufficient. On cancellation the authority
// cannot release its protection until in-flight deletion has been reconciled.
type storageGCHistoryAuthority interface {
	WithFence(context.Context, string, string, func(storageGCHistoryNativeLease) error) error
}
type storageGCHistoryNativeLease interface {
	Inventory() storageGCHistoryInventory
	Check(context.Context) error
	// Persists a global per-key retirement BEFORE local quarantine/DELETE.
	// Every future clone/restore/import/attach must install this deny before
	// serving SQL, including forks from before the local quarantine WAL.
	RegisterRetirement(context.Context, string, string, []storageGCHistoryScope) error
}
type storageGCHistoryDirectorySource interface {
	Open(context.Context, storageGCHistoryScope, string, string) (storageGCHistoryDirectoryLease, error)
}
type storageGCHistorySQLSource struct {
	Connect func(context.Context, storageGCHistoryScope) (*pgx.Conn, error)
}
type storageGCHistoryOwnedSQLLease struct {
	storageGCHistoryDirectoryLease
	conn *pgx.Conn
}

func (v *storageGCHistoryOwnedSQLLease) Close(ctx context.Context) error {
	err := v.storageGCHistoryDirectoryLease.Close(ctx)
	closed := v.conn.Close(ctx)
	if err != nil {
		return err
	}
	return closed
}
func (s storageGCHistorySQLSource) Open(ctx context.Context, scope storageGCHistoryScope, project, key string) (storageGCHistoryDirectoryLease, error) {
	if s.Connect == nil {
		return nil, errStorageGCNativeProof
	}
	conn, err := s.Connect(ctx, scope)
	if err != nil {
		return nil, err
	}
	lease, err := openStorageGCHistoryDirectory(ctx, conn, scope, project, key)
	if err != nil {
		_ = conn.Close(context.Background())
		return nil, err
	}
	return &storageGCHistoryOwnedSQLLease{storageGCHistoryDirectoryLease: lease, conn: conn}, nil
}

type unavailableStorageGCHistoryAuthority struct{}

func (unavailableStorageGCHistoryAuthority) WithFence(context.Context, string, string, func(storageGCHistoryNativeLease) error) error {
	return errStorageGCNativeProof
}

func storageGCHistoryEffectiveFloor(h storageGCHistoryHorizon) (uint64, error) {
	if !h.Complete {
		return 0, errStorageGCNativeProof
	}
	planned, a := parseLSN(h.Planned)
	applied, b := parseLSN(h.Applied)
	latest, c := parseLSN(h.Latest)
	if a != nil || b != nil || c != nil || planned > latest || applied > latest {
		return 0, errStorageGCNativeProof
	}
	floor := min(planned, applied)
	for _, pins := range [][]string{h.ChildPins, h.LeasePins, h.RestorePins, h.StandbyPins} {
		for _, value := range pins {
			lsn, err := parseLSN(value)
			if err != nil || lsn > latest {
				return 0, errStorageGCNativeProof
			}
			floor = min(floor, lsn)
		}
	}
	return floor, nil
}
func storageGCHistoryScopes(in storageGCHistoryInventory) ([]storageGCHistoryScope, error) {
	if in.Version != 1 || in.ProjectID == "" || !(catalogNativeScope{TenantID: in.TenantID, TimelineID: in.TenantID}).valid() || in.Revision == "" || in.ShardCount < 1 || in.ShardCount > 256 || !in.AllShards || !in.AllTimelineStates || !in.NoCreatingOrImporting || in.UnknownPublications || len(in.Timelines) == 0 || len(in.Timelines) > 1024 {
		return nil, errStorageGCNativeProof
	}
	seen := map[string]storageGCHistoryTimeline{}
	scopes := []storageGCHistoryScope{}
	for _, timeline := range in.Timelines {
		if _, exists := seen[timeline.TimelineID]; exists || !(catalogNativeScope{TenantID: in.TenantID, TimelineID: timeline.TimelineID}).valid() || !timeline.Readable || !timeline.AllDatabases || len(timeline.Scopes) == 0 || len(timeline.Scopes) > 1024 || len(timeline.Horizons) != in.ShardCount {
			return nil, errStorageGCNativeProof
		}
		seen[timeline.TimelineID] = timeline
		shards := map[int]bool{}
		for _, h := range timeline.Horizons {
			if h.Shard < 0 || h.Shard >= in.ShardCount || shards[h.Shard] {
				return nil, errStorageGCNativeProof
			}
			shards[h.Shard] = true
			if _, err := storageGCHistoryEffectiveFloor(h); err != nil {
				return nil, err
			}
		}
		oids := map[uint32]bool{}
		names := map[string]bool{}
		for _, scope := range timeline.Scopes {
			if !scope.valid() || scope.TenantID != in.TenantID || scope.TimelineID != timeline.TimelineID || oids[scope.DatabaseOID] || names[scope.Database] {
				return nil, errStorageGCNativeProof
			}
			oids[scope.DatabaseOID], names[scope.Database] = true, true
			scopes = append(scopes, scope)
		}
	}
	for _, timeline := range in.Timelines {
		ancestors := map[string]bool{timeline.TimelineID: true}
		parent := timeline.AncestorID
		for parent != "" {
			previous, exists := seen[parent]
			if !exists || ancestors[parent] {
				return nil, errStorageGCNativeProof
			}
			ancestors[parent] = true
			parent = previous.AncestorID
		}
	}
	sort.Slice(scopes, func(i, j int) bool {
		a, b := scopes[i], scopes[j]
		if a.TimelineID != b.TimelineID {
			return a.TimelineID < b.TimelineID
		}
		return a.DatabaseOID < b.DatabaseOID
	})
	return scopes, nil
}

func captureStorageGCHistorySnapshot(ctx context.Context, in storageGCHistoryInventory, source storageGCHistoryDirectorySource, project, key string) (storageGCHistorySnapshot, []storageGCHistoryDirectoryLease, error) {
	out := storageGCHistorySnapshot{Inventory: in, Directories: []storageGCHistoryDirectory{}}
	if !storageGCBlobKey.MatchString(key) || !strings.HasPrefix(key, project+"/") {
		return out, nil, errStorageGCHistoryIdentity
	}
	scopes, err := storageGCHistoryScopes(in)
	if err != nil || in.ProjectID != project || source == nil {
		return out, nil, errStorageGCNativeProof
	}
	leases := []storageGCHistoryDirectoryLease{}
	failed := true
	defer func() {
		if failed {
			closeStorageGCHistoryLeases(leases)
		}
	}()
	for _, scope := range scopes {
		lease, e := source.Open(ctx, scope, project, key)
		if e != nil {
			return out, nil, e
		}
		leases = append(leases, lease)
		value := lease.Observation()
		if value.Scope != scope || value.Incarnation == "" || !storageSHA256.MatchString(value.SchemaSHA256) || value.Epoch < 0 || value.ObjectReferences < 0 || value.MultipartReferences < 0 {
			return out, nil, errStorageGCHistoryIdentity
		}
		if _, e = parseLSN(value.ObservationLSN); e != nil {
			return out, nil, e
		}
		out.Directories = append(out.Directories, value)
	}
	failed = false
	return out, leases, nil
}
func closeStorageGCHistoryLeases(leases []storageGCHistoryDirectoryLease) {
	for i := len(leases) - 1; i >= 0; i-- {
		_ = leases[i].Close(context.Background())
	}
}

// A first absence snapshot alone can NEVER authorize DELETE. The second must
// prove unchanged epochs/catalog/incarnations under a current continuous fence,
// with every shard's lease/child-aware horizon strictly past the first bound.
func evaluateStorageGCHistorySnapshots(first, current storageGCHistorySnapshot) string {
	a, err := storageGCHistoryScopes(first.Inventory)
	if err != nil {
		return "baseline_native_inventory_incomplete"
	}
	b, err := storageGCHistoryScopes(current.Inventory)
	if err != nil {
		return "current_native_inventory_incomplete"
	}
	if first.Inventory.ProjectID != current.Inventory.ProjectID || first.Inventory.TenantID != current.Inventory.TenantID || first.Inventory.Revision != current.Inventory.Revision || first.Inventory.ShardCount != current.Inventory.ShardCount || len(a) != len(b) || len(first.Directories) != len(a) || len(current.Directories) != len(b) {
		return "native_topology_changed"
	}
	prior := map[storageGCHistoryScope]storageGCHistoryDirectory{}
	latest := map[storageGCHistoryScope]storageGCHistoryDirectory{}
	for _, d := range first.Directories {
		if _, ok := prior[d.Scope]; ok {
			return "duplicate_directory"
		}
		prior[d.Scope] = d
	}
	for _, d := range current.Directories {
		if _, ok := latest[d.Scope]; ok {
			return "duplicate_directory"
		}
		latest[d.Scope] = d
	}
	for i, scope := range a {
		if scope != b[i] {
			return "native_topology_changed"
		}
		x, ok := prior[scope]
		y, exists := latest[scope]
		if !ok || !exists {
			return "directory_missing"
		}
		if x.ObjectReferences != 0 || x.MultipartReferences != 0 || y.ObjectReferences != 0 || y.MultipartReferences != 0 {
			return "native_reference_present"
		}
		if x.Incarnation == "" || x.Incarnation != y.Incarnation || !storageSHA256.MatchString(x.SchemaSHA256) || x.SchemaSHA256 != y.SchemaSHA256 {
			return "native_instrumentation_changed"
		}
		if x.Epoch < 0 || x.Epoch != y.Epoch {
			return "native_reference_epoch_changed"
		}
		observed, e := parseLSN(x.ObservationLSN)
		nowLSN, f := parseLSN(y.ObservationLSN)
		if e != nil || f != nil || observed == 0 || nowLSN < observed {
			return "native_observation_lsn_invalid"
		}
		for _, timeline := range current.Inventory.Timelines {
			if timeline.TimelineID != scope.TimelineID {
				continue
			}
			for _, h := range timeline.Horizons {
				floor, e := storageGCHistoryEffectiveFloor(h)
				latest, _ := parseLSN(h.Latest)
				if e != nil || floor <= observed || latest < nowLSN {
					return "native_retention_or_pin_held"
				}
			}
		}
	}
	return "eligible"
}

var errStorageGCHistoryHeld = errors.New("Storage published history remains protected")
