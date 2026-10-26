package control

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTimeTravelPreviewNeverPinsAndRejectsUnknownHistory(t *testing.T) {
	var kind, point atomic.Value
	var calls atomic.Int64
	kind.Store("present")
	point.Store("0/2000")
	when := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || strings.HasSuffix(r.URL.Path, "/lsn_lease") {
			t.Error("preview attempted a storage mutation", r.Method, r.URL.Path)
		}
		if strings.HasSuffix(r.URL.Path, "/get_lsn_by_timestamp") {
			if r.URL.Query().Get("timestamp") != when.Format(time.RFC3339Nano) {
				t.Error("timestamp scope not normalized")
			}
			jsonResponse(w, 200, record{"kind": kind.Load().(string), "lsn": point.Load().(string)})
		} else {
			jsonResponse(w, 200, record{"min_readable_lsn": "0/1000", "initdb_lsn": "0/1000", "last_record_lsn": "0/4000"})
		}
	}))
	defer peer.Close()
	k := &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}
	timestamp := when.In(time.FixedZone("preview-offset", 2*3600)).Format(time.RFC3339)
	value, err := k.observeRestorePoint(context.Background(), strings.Repeat("a", 32), strings.Repeat("b", 32), timestamp, "")
	if err != nil || value.Pinned || value.LSN != "0/2000" || value.Timestamp != when.Format(time.RFC3339) || value.Window.MinimumLSN != "0/1000" {
		t.Fatal("wrong original preview", value, err)
	}
	for _, test := range []struct{ kind, code string }{{"past", "restore_point_outside_history"}, {"future", "restore_timestamp_not_replayed"}, {"nodata", "restore_history_empty"}} {
		kind.Store(test.kind)
		_, err = k.observeRestorePoint(context.Background(), "tenant", "timeline", timestamp, "")
		var fault restoreError
		if !errors.As(err, &fault) || fault.code != test.code {
			t.Fatal(test, err)
		}
	}
	for _, invalid := range []string{"unknown", ""} {
		kind.Store(invalid)
		if _, err = k.observeRestorePoint(context.Background(), "tenant", "timeline", timestamp, ""); err == nil {
			t.Fatal("unknown history accepted")
		}
	}
	kind.Store("present")
	for _, invalid := range []string{"0/5000", "0/FFF", "0/0", "broken"} {
		point.Store(invalid)
		if _, err = k.observeRestorePoint(context.Background(), "tenant", "timeline", timestamp, ""); err == nil {
			t.Fatal("invalid storage timestamp result accepted", invalid)
		}
	}
	before := calls.Load()
	for _, input := range [][2]string{{"", ""}, {timestamp, "0/2000"}, {"", "0/1001"}, {"", "0/2000?other=x"}, {time.Now().Add(time.Hour).Format(time.RFC3339), ""}} {
		if _, err = k.observeRestorePoint(context.Background(), "tenant", "timeline", input[0], input[1]); err == nil {
			t.Fatal("invalid client preview accepted", input)
		}
	}
	if calls.Load() != before {
		t.Fatal("invalid input caused storage reads")
	}
	value, err = k.observeRestorePoint(context.Background(), "tenant", "original-timeline", "", "0/00002000")
	if err != nil || value.TimelineID != "original-timeline" || value.LSN != "0/2000" || value.Timestamp != "" || value.Pinned {
		t.Fatal(value, err)
	}
}
