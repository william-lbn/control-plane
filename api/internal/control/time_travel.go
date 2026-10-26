package control

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

var restoreTimelineID = regexp.MustCompile(`^[a-f0-9]{32}$`)

// A preview never pins history, wakes Compute, or changes a branch. Acceptance
// must recheck this exact timeline and lease the exact resolved LSN.
type restorePointObservation struct {
	BranchID   string        `json:"branch_id"`
	TimelineID string        `json:"source_timeline_id"`
	LSN        string        `json:"lsn"`
	Timestamp  string        `json:"timestamp,omitempty"`
	Window     restoreWindow `json:"window"`
	Pinned     bool          `json:"retention_pinned"`
}

func (k *kubeClient) observeRestorePoint(ctx context.Context, tenant, timeline, timestamp, lsn string) (restorePointObservation, error) {
	value := restorePointObservation{TimelineID: timeline}
	if timestamp == "" && lsn == "" {
		return value, restoreError{422, "restore_point_required"}
	}
	if err := validateRestoreInput(timestamp, lsn, time.Now()); err != nil {
		return value, err
	}
	window, err := k.timelineRestoreWindow(ctx, tenant, timeline)
	if err != nil {
		return value, err
	}
	value.Window = window
	if timestamp != "" {
		t, _ := time.Parse(time.RFC3339Nano, timestamp)
		value.Timestamp = t.UTC().Format(time.RFC3339Nano)
		point, err := k.serviceRequest(ctx, "pageserver-managed", 9898,
			"v1/tenant/"+tenant+"/timeline/"+timeline+"/get_lsn_by_timestamp?timestamp="+url.QueryEscape(value.Timestamp), http.MethodGet, nil)
		if err != nil {
			return value, err
		}
		switch stringVal(point["kind"]) {
		case "present":
			lsn = stringVal(point["lsn"])
		case "past":
			return value, restoreError{422, "restore_point_outside_history"}
		case "future":
			return value, restoreError{409, "restore_timestamp_not_replayed"}
		case "nodata":
			return value, restoreError{422, "restore_history_empty"}
		default:
			return value, errors.New("unknown storage timestamp result")
		}
	}
	value.LSN, err = canonicalLSN(lsn)
	if err != nil {
		return value, err
	}
	if err = validateRestoreInput("", value.LSN, time.Now()); err != nil {
		return value, errors.New("invalid storage restore point")
	}
	if err = restoreLSNWithinWindow(value.LSN, window); err != nil {
		return value, err
	}
	return value, nil
}

func (s *server) previewRestorePoint(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !pitrEnabled() {
		fail(w, r, 503, "pitr_disabled", "Historical branch restore is not enabled")
		return
	}
	query := r.URL.Query()
	for key, values := range query {
		if (key != "timestamp" && key != "lsn") || len(values) != 1 || values[0] == "" {
			fail(w, r, 422, "invalid_restore_preview", "Specify exactly one timestamp or LSN")
			return
		}
	}
	branch, err := s.one(r.Context(), `SELECT b.timeline_id,p.tenant_id FROM branches b JOIN projects p ON p.id=b.project_id WHERE b.id=$1 AND p.id=$2 AND b.deleted_at IS NULL AND b.state='ready' AND p.deleted_at IS NULL AND p.state='ready' AND p.source='managed'`, r.PathValue("branch"), r.PathValue("project"))
	if err != nil {
		if isNoRows(err) {
			fail(w, r, 404, "branch_not_found", "Ready managed branch not found")
		} else {
			fail(w, r, 503, "metadata_unavailable", "Could not inspect original source timeline")
		}
		return
	}
	value, err := s.kube.observeRestorePoint(r.Context(), stringVal(branch["tenant_id"]), stringVal(branch["timeline_id"]), query.Get("timestamp"), query.Get("lsn"))
	if err != nil {
		var point restoreError
		if errors.As(err, &point) {
			fail(w, r, point.status, point.code, "The requested restore point is not available")
		} else {
			fail(w, r, 503, "restore_point_unavailable", "Could not observe the requested restore point")
		}
		return
	}
	// Source may have changed during storage I/O. Never label another timeline's
	// result as the current branch preview, even if its numeric LSN also exists.
	var same bool
	err = s.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM branches b JOIN projects p ON p.id=b.project_id WHERE b.id=$1 AND p.id=$2 AND b.timeline_id=$3 AND p.tenant_id=$4 AND b.state='ready' AND p.state='ready' AND b.deleted_at IS NULL AND p.deleted_at IS NULL)`, r.PathValue("branch"), r.PathValue("project"), value.TimelineID, branch["tenant_id"]).Scan(&same)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not confirm original source timeline")
		return
	}
	if !same {
		fail(w, r, 412, "restore_source_changed", "Inspect the current branch history again")
		return
	}
	value.BranchID, value.Window.BranchID = r.PathValue("branch"), r.PathValue("branch")
	jsonResponse(w, 200, value)
}
