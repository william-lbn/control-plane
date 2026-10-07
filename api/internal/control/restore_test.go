package control

import (
	"errors"
	"testing"
	"time"
)

func TestRestorePointValidation(t *testing.T) {
	now := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		timestamp, lsn string
		valid          bool
	}{
		{"", "", true}, {"2026-10-07T09:00:00.123456Z", "", true}, {"2026-10-07T11:00:00+02:00", "", true},
		{"", "ABCDEF01/00001000", true}, {"2026-10-07T10:00:01Z", "", false}, {"2026-10-07T09:00:00", "", false},
		{"2026-10-07T09:00:00Z", "0/1000", false}, {"", "0/0", false}, {"", "100000000/0", false}, {"", "0/1001", false},
		{"", "0/1000?timestamp=other", false}, {"1999-12-31T23:59:59Z", "", false},
	} {
		if (validateRestoreInput(tc.timestamp, tc.lsn, now) == nil) != tc.valid {
			t.Errorf("unexpected validity for %q/%q", tc.timestamp, tc.lsn)
		}
	}
}

func TestRestoreWindowUsesNumericLSNs(t *testing.T) {
	window := restoreWindow{MinimumLSN: "0/FFFFF0", LatestLSN: "1/10"}
	for _, lsn := range []string{"0/FFFFF0", "0/FFFFFF", "1/0", "1/10"} {
		if err := restoreLSNWithinWindow(lsn, window); err != nil {
			t.Fatal(lsn, err)
		}
	}
	for _, lsn := range []string{"0/FFFFE8", "1/18"} {
		var unavailable restoreError
		if err := restoreLSNWithinWindow(lsn, window); !errors.As(err, &unavailable) || unavailable.code != "restore_point_outside_history" {
			t.Fatal(lsn, err)
		}
	}
	got, err := canonicalLSN("abcdef01/00001000")
	if err != nil || got != "ABCDEF01/1000" {
		t.Fatal(got, err)
	}
}

func TestRestoreTimelineReplayRequiresExactAncestry(t *testing.T) {
	if err := validateTimelineAncestor(map[string]any{"ancestor_timeline_id": "parent", "ancestor_lsn": "0/01000"}, "parent", "0/1000"); err != nil {
		t.Fatal(err)
	}
	for _, doc := range []map[string]any{{"ancestor_timeline_id": "other", "ancestor_lsn": "0/1000"}, {"ancestor_timeline_id": "parent", "ancestor_lsn": "0/1008"}, {"ancestor_timeline_id": "parent"}, {}} {
		if err := validateTimelineAncestor(doc, "parent", "0/1000"); err == nil {
			t.Fatal("accepted mismatched historical child", doc)
		}
	}
	if len(branchCreationSteps(false, true)) != 3 || len(branchCreationSteps(true, true)) != 7 || len(branchCreationSteps(true, false)) != 6 {
		t.Fatal("restore step contract mismatch")
	}
}
