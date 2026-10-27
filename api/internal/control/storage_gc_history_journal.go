package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
)

type storageGCHistoryAllocation struct {
	project, tenant, key, id string
	size                     int64
}

func (j *storageGCJournal) historyAllocation(ctx context.Context, project, key, actor string) (storageGCHistoryAllocation, error) {
	v := storageGCHistoryAllocation{project: project, key: key}
	if j == nil || j.db == nil || j.deleter == nil || !j.deleter.operators[actor] || !j.deleter.projects[project] || !storageGCBlobKey.MatchString(key) {
		return v, errStorageGCHistoryHeld
	}
	err := j.db.QueryRow(ctx, `SELECT a.allocation_id,a.size,p.tenant_id FROM storage_gc_allocations a JOIN projects p ON p.id=a.project_id JOIN users u ON u.id=$4 AND NOT u.disabled WHERE a.store_id=$1 AND a.blob_key=$2 AND a.project_id=$3 AND a.state='may_reference'`, j.storeID, key, project, actor).Scan(&v.id, &v.size, &v.tenant)
	if err != nil {
		return v, err
	}
	if !(catalogNativeScope{TenantID: v.tenant, TimelineID: v.tenant}).valid() {
		return v, errStorageGCHistoryHeld
	}
	return v, nil
}
func (j *storageGCJournal) recordHistory(ctx context.Context, v storageGCHistoryAllocation, actor, baseline, kind, outcome, reason string, snapshot storageGCHistorySnapshot) (string, error) {
	data, err := json.Marshal(snapshot)
	if err != nil || len(data) > 1<<20 {
		return "", errStorageGCHistoryIdentity
	}
	hash := sha256.Sum256(data)
	id := newID("sgh_")
	_, err = j.allocationDB().Exec(ctx, `INSERT INTO storage_gc_history_evidence(id,baseline_id,store_id,blob_key,project_id,actor_id,kind,outcome,reason,snapshot,snapshot_sha256) VALUES($1,NULLIF($2,''),$3,$4,$5,$6,$7,$8,$9,$10,$11)`, id, baseline, j.storeID, v.key, v.project, actor, kind, outcome, reason, data, hex.EncodeToString(hash[:]))
	return id, err
}

// prepareHistory creates actual first-absence evidence using SQL epoch/table
// locks. It does not interpret a current empty manifest as historical absence.
// Native authority unavailable produces retained blocked evidence, never a plan.
func (j *storageGCJournal) prepareHistory(ctx context.Context, project, key, actor string, authority storageGCHistoryAuthority, source storageGCHistoryDirectorySource) (string, error) {
	v, err := j.historyAllocation(ctx, project, key, actor)
	if err != nil {
		return "", err
	}
	if authority == nil {
		authority = unavailableStorageGCHistoryAuthority{}
	}
	id := ""
	err = authority.WithFence(ctx, project, v.tenant, func(native storageGCHistoryNativeLease) error {
		in := native.Inventory()
		if in.TenantID != v.tenant || native.Check(ctx) != nil {
			return errStorageGCNativeProof
		}
		snapshot, leases, e := captureStorageGCHistorySnapshot(ctx, in, source, project, key)
		if e != nil {
			return e
		}
		defer closeStorageGCHistoryLeases(leases)
		for _, lease := range leases {
			if e = lease.Check(ctx); e != nil {
				return e
			}
		}
		if e = native.Check(ctx); e != nil {
			return e
		}
		reason, outcome := "absence_observed_not_history_proven", "baseline"
		for _, d := range snapshot.Directories {
			if d.ObjectReferences != 0 || d.MultipartReferences != 0 {
				reason, outcome = "native_reference_present", "held"
			}
		}
		id, e = j.recordHistory(ctx, v, actor, "", "baseline", outcome, reason, snapshot)
		return e
	})
	if id == "" {
		var recordErr error
		id, recordErr = j.recordHistory(ctx, v, actor, "", "blocked", "held", "native_or_directory_authority_unavailable", storageGCHistorySnapshot{})
		if recordErr != nil {
			return "", recordErr
		}
		return id, errStorageGCHistoryHeld
	}
	return id, err
}

// executeHistory is a complete fail-closed pipeline for an INTERNAL native
// provider. No production provider exists yet and no public route activates it.
// It preserves every 071 allocation terminal state and all immutable evidence.
func (j *storageGCJournal) executeHistory(ctx context.Context, baseline, actor string, authority storageGCHistoryAuthority, source storageGCHistoryDirectorySource) (string, error) {
	if j == nil || j.db == nil || j.deleter == nil {
		return "", errStorageGCHistoryHeld
	}
	if !regexp.MustCompile(`^sgh_[a-f0-9]{24}$`).MatchString(baseline) {
		return "", errStorageGCHistoryHeld
	}
	var project, key string
	var raw []byte
	if err := j.db.QueryRow(ctx, `SELECT project_id,blob_key,snapshot FROM storage_gc_history_evidence WHERE id=$1 AND store_id=$2 AND kind='baseline' AND outcome='baseline'`, baseline, j.storeID).Scan(&project, &key, &raw); err != nil {
		return "", err
	}
	v, err := j.historyAllocation(ctx, project, key, actor)
	if err != nil {
		return "", err
	}
	guard, err := j.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer guard.Rollback(context.Background())
	// All existing API/gateway blob I/O holds this project's FOR SHARE row
	// lock. NO KEY UPDATE drains those readers/writers and closes new admission
	// while allowing the separate evidence connection's FK KEY SHARE checks.
	var lockedProject, lockedActor, allocationState string
	if err = guard.QueryRow(ctx, `SELECT id FROM projects WHERE id=$1 FOR NO KEY UPDATE`, project).Scan(&lockedProject); err != nil {
		return "", err
	}
	if err = guard.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 AND NOT disabled FOR SHARE`, actor).Scan(&lockedActor); err != nil {
		return "", err
	}
	if err = guard.QueryRow(ctx, `SELECT state FROM storage_gc_allocations WHERE store_id=$1 AND blob_key=$2 FOR NO KEY UPDATE`, j.storeID, key).Scan(&allocationState); err != nil || allocationState != "may_reference" {
		return "", errStorageGCHistoryHeld
	}
	var first storageGCHistorySnapshot
	if json.Unmarshal(raw, &first) != nil {
		return "", errStorageGCHistoryIdentity
	}
	if authority == nil {
		authority = unavailableStorageGCHistoryAuthority{}
	}
	result := ""
	reason := "native_or_directory_authority_unavailable"
	err = authority.WithFence(ctx, project, v.tenant, func(native storageGCHistoryNativeLease) error {
		if native.Inventory().TenantID != v.tenant || native.Check(ctx) != nil {
			return errStorageGCNativeProof
		}
		current, leases, e := captureStorageGCHistorySnapshot(ctx, native.Inventory(), source, project, key)
		if e != nil {
			return e
		}
		defer closeStorageGCHistoryLeases(leases)
		reason = evaluateStorageGCHistorySnapshots(first, current)
		if reason != "eligible" {
			_, e = j.recordHistory(ctx, v, actor, baseline, "check", "held", reason, current)
			if e != nil {
				return e
			}
			return errStorageGCHistoryHeld
		}
		for _, lease := range leases {
			if e = lease.Check(ctx); e != nil {
				return e
			}
		}
		if e = native.Check(ctx); e != nil {
			return e
		}
		if e = j.deleter.backing.versioningDisabled(ctx); e != nil {
			reason = "backing_versioning_unverified"
			return errStorageGCHistoryHeld
		}
		if _, e = j.recordHistory(ctx, v, actor, baseline, "intent", "delete_unknown", "published_retirement_intent", current); e != nil {
			return e
		}
		scopes, e := storageGCHistoryScopes(current.Inventory)
		if e != nil {
			return e
		}
		if e = native.RegisterRetirement(ctx, key, baseline, scopes); e != nil {
			reason = "native_retirement_unconfirmed"
			return errStorageGCHistoryHeld
		}
		for _, lease := range leases {
			if e = lease.Quarantine(ctx, baseline); e != nil {
				reason = "native_quarantine_incomplete"
				return errStorageGCHistoryHeld
			}
		}
		// The permanent local deny survives SQL lock loss. The native global
		// retirement MUST also cover all future timeline births before DELETE.
		if e = native.Check(ctx); e != nil {
			reason = "native_fence_lost_after_quarantine"
			return errStorageGCHistoryHeld
		}
		state, backingReason := storageGCCollectObject(ctx, j.deleter.backing, key, v.id, v.size)
		if _, e = j.recordHistory(ctx, v, actor, baseline, "result", state, backingReason, current); e != nil {
			return e
		}
		result = state
		return nil
	})
	if err != nil {
		if _, recordErr := j.recordHistory(ctx, v, actor, baseline, "blocked", "held", reason, storageGCHistorySnapshot{}); recordErr != nil {
			return "", recordErr
		}
		return "held", err
	}
	if result == "" {
		return "held", errors.New("native history authority returned without completing fenced work")
	}
	return result, nil
}
