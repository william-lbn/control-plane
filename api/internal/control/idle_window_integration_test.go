package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type idleGenerationTransport func(*http.Request) (*http.Response, error)

func (f idleGenerationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The lock is real PostgreSQL; Kubernetes models a wake that changed UID after
// idle evidence was queued. No SQL probe or DELETE may be sent to the new VM.
func TestAutomaticSuspendRejectsChangedUID(t *testing.T) {
	url := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("NEON_V2_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	project, endpoint := "prj_idle_generation", "ep_0123456789abcdef"
	name := kubeName(endpoint)
	requests := 0
	route := map[string]any{"project_id": project, "kind": "neonvm", "workload": name,
		"vm_template": map[string]any{"metadata": map[string]any{"name": name, "labels": map[string]any{"neon-control/endpoint-id": endpoint}}}}
	raw, _ := json.Marshal(map[string]any{selector(endpoint): route})
	kube := &kubeClient{base: "http://kube.invalid", namespace: "neon"}
	kube.http = &http.Client{Transport: idleGenerationTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != "GET" {
			t.Fatalf("changed VM mutated: %s", r.Method)
		}
		var body any
		switch r.URL.Path {
		case kube.path("secret", routesSecret):
			body = map[string]any{"data": map[string]any{"routes.json": base64.StdEncoding.EncodeToString(raw)}}
		case kube.path("vm", name):
			body = map[string]any{"metadata": map[string]any{"uid": "new-wake-uid", "labels": map[string]any{"neon-control/project-id": project, "neon-control/endpoint-id": endpoint}}, "status": map[string]any{"phase": "Running"}}
		default:
			t.Fatalf("new generation probed unexpectedly: %s", r.URL.Path)
		}
		encoded, _ := json.Marshal(body)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(encoded))}, nil
	})}
	s := server{db: pool, kube: kube}
	err = s.suspendCompute(ctx, suspendPayload{ProjectID: project, EndpointID: endpoint, WorkloadName: name, ExpectedVMUID: "old-idle-uid"})
	if err == nil || !strings.Contains(err.Error(), "generation changed") || requests != 2 {
		t.Fatalf("old idle evidence was not rejected before probing: error=%v calls=%d", err, requests)
	}
}

// Verify the real PostgreSQL operator, nullable scanner and generation bounds.
// This isolated schema has only the columns used by idle evidence; it is not
// a migration/HA test. Keep all test rows for inspection, never DROP a schema.
func TestIdleWindowPostgresObservations(t *testing.T) {
	url, schema := os.Getenv("NEON_V2_TEST_DATABASE_URL"), os.Getenv("NEON_V2_TEST_SCHEMA")
	if isolated := os.Getenv("NEON_V2_TEST_IDLE_SCHEMA"); isolated != "" {
		schema = isolated
	}
	if url == "" {
		t.Skip("NEON_V2_TEST_DATABASE_URL not set")
	}
	if !regexp.MustCompile(`^v2_migration_idle_[a-z0-9_]+$`).MatchString(schema) {
		t.Fatal("Dedicated v2_migration_idle_* schema required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err = pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS metric_samples(endpoint_id text,sampled_at timestamptz,
 observed_state text,connections int,errors jsonb)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	born := now.Add(-time.Hour)
	s := server{db: pool}
	for _, scenario := range []string{"valid", "null", "postgres_error", "suspended", "new_generation"} {
		t.Run(scenario, func(t *testing.T) {
			endpoint := newID("ep_idle_")
			for i, age := range []int{75, 45, 15} {
				state, errors := "active", "{}"
				var connections any = 0
				if i == 1 {
					switch scenario {
					case "null":
						connections = nil
					case "postgres_error":
						errors = `{"postgres":"query_failed"}`
					case "suspended":
						state = "suspended"
					}
				}
				if _, err := pool.Exec(ctx, `INSERT INTO metric_samples VALUES($1,$2,$3,$4,$5)`, endpoint, now.Add(-time.Duration(age)*time.Second), state, connections, errors); err != nil {
					t.Fatal(err)
				}
			}
			generationBorn := born
			if scenario == "new_generation" {
				generationBorn = now.Add(-35 * time.Second)
			}
			samples, err := s.readIdleSamples(ctx, endpoint, generationBorn, 60)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "new_generation" && len(samples) != 1 {
				t.Fatal("prior generation samples not excluded")
			}
			if got := idleWindowEligible(samples, now, generationBorn, time.Minute); got != (scenario == "valid") {
				t.Fatalf("eligibility=%v for %s", got, scenario)
			}
		})
	}
}
