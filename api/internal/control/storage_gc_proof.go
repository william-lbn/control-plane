package control

import (
	"context"
	_ "embed"
	"errors"
	"regexp"
	"strings"
)

//go:embed assets/storage-gc.sql
var storageGCReferenceSQL string

var errStorageGCNativeProof = errors.New("complete native timeline/history reference proof unavailable")

// This is an internal adapter contract, never an HTTP request shape. The
// deployed pageserver API supplies min_readable_lsn, but does not currently
// supply an exhaustive, fenced inventory of all retained and detached timelines
// and their database directories. Consequently no production adapter is enabled
// and published allocations cannot become orphan candidates through this file.
type storageGCNativeProof struct {
	ProjectID, TenantID, BlobKey string
	// Obtained from the native storage authority, not control metadata. The
	// authority must hold the fence through physical DELETE, blocking restore,
	// clone, reset, new publication and new leases below the proven horizons.
	NativeInventoryRevision string
	FenceHeld               bool
	AllTimelinesEnumerated  bool
	UnknownPublications     bool
	Timelines               []storageGCTimelineProof
}

type storageGCTimelineProof struct {
	TimelineID, AncestorID string
	// NoReferencesLSN is an earlier, committed, complete directory observation
	// made AFTER the last possible publish/unknown result for this candidate.
	// MinimumReadableLSN includes every existing retention/PITR/child/lease pin.
	NoReferencesLSN, MinimumReadableLSN, LatestLSN string
	AllDatabasesEnumerated                         bool
	Readable                                       bool
	Directories                                    []storageGCDirectoryProof
}

type storageGCDirectoryProof struct {
	DatabaseOID      uint32
	Database         string
	IdentityVerified bool
	Complete         bool
	// Count every reference, including copied/inherited objects and ALL
	// multipart_parts rows. Aborted/completed upload parts remain references.
	ObjectReferences, MultipartReferences, OtherReferences uint64
	UnknownOutcomes                                        bool
}

// There is intentionally no timestamp/TTL fallback, metadata-branch-only
// adapter, operator-supplied proof body, or switch that bypasses native proof.
func (s *server) nativeStorageGCProof(ctx context.Context, project, key string) (storageGCNativeProof, error) {
	return storageGCNativeProof{}, errStorageGCNativeProof
}

// validateStorageGCNativeProof is useful to implement and test the native
// adapter without enabling deletion prematurely. The orphan collector never
// calls it and never changes a may_reference allocation into never_published.
func validateStorageGCNativeProof(p storageGCNativeProof) error {
	if !storageGCBlobKey.MatchString(p.BlobKey) || strings.SplitN(p.BlobKey, "/", 2)[0] != p.ProjectID || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(p.TenantID) || p.NativeInventoryRevision == "" || !p.FenceHeld || !p.AllTimelinesEnumerated || p.UnknownPublications || len(p.Timelines) == 0 {
		return errStorageGCNativeProof
	}
	seen := map[string]bool{}
	for _, t := range p.Timelines {
		if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(t.TimelineID) || seen[t.TimelineID] || !t.Readable || !t.AllDatabasesEnumerated || len(t.Directories) == 0 {
			return errStorageGCNativeProof
		}
		seen[t.TimelineID] = true
		observed, a := parseLSN(t.NoReferencesLSN)
		minimum, b := parseLSN(t.MinimumReadableLSN)
		latest, c := parseLSN(t.LatestLSN)
		if a != nil || b != nil || c != nil || observed == 0 || minimum <= observed || latest < minimum {
			return errStorageGCNativeProof
		}
		databases := map[uint32]bool{}
		names := map[string]bool{}
		for _, d := range t.Directories {
			if d.DatabaseOID == 0 || !validPGIdentifier(d.Database) || databases[d.DatabaseOID] || names[d.Database] || !d.IdentityVerified || !d.Complete || d.UnknownOutcomes || d.ObjectReferences != 0 || d.MultipartReferences != 0 || d.OtherReferences != 0 {
				return errStorageGCNativeProof
			}
			databases[d.DatabaseOID], names[d.Database] = true, true
		}
	}
	for _, t := range p.Timelines {
		if t.AncestorID != "" && (!seen[t.AncestorID] || t.AncestorID == t.TimelineID) {
			return errStorageGCNativeProof
		}
	}
	for _, t := range p.Timelines {
		// Reject ancestry cycles, including cycles longer than one generation.
		ancestors := map[string]bool{t.TimelineID: true}
		ancestor := t.AncestorID
		for ancestor != "" {
			if ancestors[ancestor] {
				return errStorageGCNativeProof
			}
			ancestors[ancestor] = true
			for _, candidate := range p.Timelines {
				if candidate.TimelineID == ancestor {
					ancestor = candidate.AncestorID
					break
				}
			}
		}
	}
	return nil
}
