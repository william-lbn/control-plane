package control

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestFunctionArtifactMetadataAvailability(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgxpool.ParseConfig(dsn)
	if dsn == "" || err != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("dedicated control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "v2_artifact_availability_" + randomToken(6)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	state := &controllerState{}
	state.leading.Store(true)
	g := &functionArtifactGateway{server: &server{db: db, functions: &functionsPlatform{}}, leadership: state, budget: make(chan struct{}, 1)}
	call := func(want int, code string) {
		t.Helper()
		r := httptest.NewRequest("GET", "/internal/v1/function-artifacts/fni_0123456789abcdef/fdp_0123456789abcdef/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r.WithContext(ctx))
		var result struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &result)
		if w.Code != want || result.Code != code {
			t.Fatal("metadata error classification", w.Code, w.Body.String())
		}
		if want == 503 && w.Header().Get("Retry-After") != "1" {
			t.Fatal("retry guidance missing")
		}
	}
	call(404, "artifact_identity_rejected")
	db.Close()
	call(503, "artifact_metadata_unavailable")
}
