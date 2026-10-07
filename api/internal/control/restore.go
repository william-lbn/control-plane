package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Restore creates a distinct timeline. It never repoints an existing Endpoint,
// rewinds the control catalog, or resurrects a revoked platform credential.
func pitrEnabled() bool { return creationEnabled() && os.Getenv("NEON_V2_PITR_ENABLED") == "true" }

var pgLSNPattern = regexp.MustCompile(`^[0-9A-Fa-f]{1,8}/[0-9A-Fa-f]{1,8}$`)

func parseLSN(value string) (uint64, error) {
	if !pgLSNPattern.MatchString(value) {
		return 0, errors.New("invalid PostgreSQL LSN")
	}
	parts := strings.Split(value, "/")
	hi, _ := strconv.ParseUint(parts[0], 16, 32)
	lo, _ := strconv.ParseUint(parts[1], 16, 32)
	return hi<<32 | lo, nil
}

func canonicalLSN(value string) (string, error) {
	n, err := parseLSN(value)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%X/%X", n>>32, n&0xffffffff), nil
}

type restoreError struct {
	status int
	code   string
}

func (e restoreError) Error() string { return e.code }

type restoreWindow struct {
	BranchID   string    `json:"branch_id"`
	MinimumLSN string    `json:"min_readable_lsn"`
	LatestLSN  string    `json:"latest_lsn"`
	ObservedAt time.Time `json:"observed_at"`
}

func (k *kubeClient) timelineRestoreWindow(ctx context.Context, tenant, timeline string) (restoreWindow, error) {
	item, err := k.serviceRequest(ctx, "pageserver-managed", 9898,
		"v1/tenant/"+tenant+"/timeline/"+timeline, http.MethodGet, nil)
	if err != nil {
		return restoreWindow{}, err
	}
	// min_readable_lsn includes the effective retention boundary; the applied GC
	// cutoff alone can advertise data that is already eligible for collection.
	minimum, err := canonicalLSN(stringVal(item["min_readable_lsn"]))
	if err != nil {
		return restoreWindow{}, errors.New("storage retention boundary unavailable")
	}
	if initdb := stringVal(item["initdb_lsn"]); initdb != "" {
		n, err := parseLSN(initdb)
		if err != nil {
			return restoreWindow{}, err
		}
		m, _ := parseLSN(minimum)
		if n > m {
			minimum, _ = canonicalLSN(initdb)
		}
	}
	latest, err := canonicalLSN(stringVal(item["last_record_lsn"]))
	if err != nil {
		return restoreWindow{}, err
	}
	lo, _ := parseLSN(minimum)
	hi, _ := parseLSN(latest)
	if lo > hi {
		return restoreWindow{}, errors.New("invalid storage restore window")
	}
	return restoreWindow{MinimumLSN: minimum, LatestLSN: latest, ObservedAt: time.Now().UTC()}, nil
}

func (s *server) readRestoreWindow(w http.ResponseWriter, r *http.Request) {
	if !pitrEnabled() {
		fail(w, r, 503, "pitr_disabled", "Historical branch restore is not enabled")
		return
	}
	branch, err := s.one(r.Context(), `SELECT b.id,b.timeline_id,p.tenant_id FROM branches b JOIN projects p ON p.id=b.project_id
  WHERE b.id=$1 AND b.project_id=$2 AND b.deleted_at IS NULL AND b.state='ready' AND p.source='managed' AND p.state='ready'`,
		r.PathValue("branch"), r.PathValue("project"))
	if err != nil {
		if isNoRows(err) {
			fail(w, r, 404, "branch_not_found", "Ready managed branch not found")
		} else {
			fail(w, r, 503, "metadata_unavailable", "Could not read branch")
		}
		return
	}
	window, err := s.kube.timelineRestoreWindow(r.Context(), stringVal(branch["tenant_id"]), stringVal(branch["timeline_id"]))
	if err != nil {
		fail(w, r, 503, "restore_window_unavailable", "Could not read storage retention window")
		return
	}
	window.BranchID = stringVal(branch["id"])
	jsonResponse(w, 200, window)
}

func validateRestoreInput(timestamp, lsn string, now time.Time) error {
	if timestamp != "" && lsn != "" {
		return restoreError{422, "restore_point_conflict"}
	}
	if timestamp != "" {
		t, err := time.Parse(time.RFC3339Nano, timestamp)
		if err != nil || t.Year() < 2000 {
			return restoreError{422, "invalid_restore_timestamp"}
		}
		if t.After(now) {
			return restoreError{422, "restore_timestamp_in_future"}
		}
	}
	if lsn != "" {
		n, err := parseLSN(lsn)
		if err != nil || n == 0 || n%8 != 0 {
			return restoreError{422, "invalid_restore_lsn"}
		}
	}
	return nil
}

func restoreLSNWithinWindow(lsn string, window restoreWindow) error {
	n, err := parseLSN(lsn)
	if err != nil {
		return err
	}
	minimum, err := parseLSN(window.MinimumLSN)
	if err != nil {
		return err
	}
	latest, err := parseLSN(window.LatestLSN)
	if err != nil {
		return err
	}
	if n < minimum || n > latest {
		return restoreError{422, "restore_point_outside_history"}
	}
	return nil
}

func (k *kubeClient) pinRestoreLSN(ctx context.Context, tenant, timeline, lsn string) error {
	lease, err := k.serviceRequest(ctx, "pageserver-managed", 9898,
		"v1/tenant/"+tenant+"/timeline/"+timeline+"/lsn_lease", http.MethodPost, map[string]string{"lsn": lsn})
	if err != nil {
		return err
	}
	until, err := time.Parse(time.RFC3339Nano, stringVal(lease["valid_until"]))
	if err != nil || !until.After(time.Now().Add(5*time.Second)) {
		return errors.New("storage restore lease unavailable or too short")
	}
	return nil
}

func (k *kubeClient) resolveRestorePoint(ctx context.Context, tenant, timeline, timestamp, lsn string) (string, error) {
	window, err := k.timelineRestoreWindow(ctx, tenant, timeline)
	if err != nil {
		return "", err
	}
	if timestamp != "" {
		t, _ := time.Parse(time.RFC3339Nano, timestamp)
		point, err := k.serviceRequest(ctx, "pageserver-managed", 9898,
			"v1/tenant/"+tenant+"/timeline/"+timeline+"/get_lsn_by_timestamp?timestamp="+url.QueryEscape(t.UTC().Format(time.RFC3339Nano)), http.MethodGet, nil)
		if err != nil {
			return "", err
		}
		switch stringVal(point["kind"]) {
		case "present":
			lsn = stringVal(point["lsn"])
		case "past":
			return "", restoreError{422, "restore_point_outside_history"}
		case "future":
			return "", restoreError{409, "restore_timestamp_not_replayed"}
		case "nodata":
			return "", restoreError{422, "restore_history_empty"}
		default:
			return "", errors.New("unknown storage timestamp result")
		}
	}
	lsn, err = canonicalLSN(lsn)
	if err != nil {
		return "", err
	}
	if err = restoreLSNWithinWindow(lsn, window); err != nil {
		return "", err
	}
	// Resolving a point is insufficient: pin it before accepting durable intent.
	// The Worker renews this exact LSN; it never resolves the timestamp again.
	if err = k.pinRestoreLSN(ctx, tenant, timeline, lsn); err != nil {
		return "", err
	}
	return lsn, nil
}

func branchCreationSteps(withEndpoint, historical bool) []string {
	steps := createSteps("create_branch", withEndpoint)
	if historical {
		return append([]string{"pin_restore_point"}, steps...)
	}
	return steps
}

func validateTimelineAncestor(item map[string]any, parentID, parentLSN string) error {
	if parentID == "" {
		if stringVal(item["ancestor_timeline_id"]) != "" {
			return errors.New("root timeline ancestry mismatch")
		}
		return nil
	}
	if stringVal(item["ancestor_timeline_id"]) != parentID {
		return errors.New("timeline parent ownership mismatch")
	}
	actual, err := parseLSN(stringVal(item["ancestor_lsn"]))
	if err != nil {
		return errors.New("timeline ancestor LSN unavailable")
	}
	expected, err := parseLSN(parentLSN)
	if err != nil || actual != expected {
		return errors.New("timeline restore point mismatch")
	}
	return nil
}
