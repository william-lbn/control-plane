package dataapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// This is a real PostgREST/PG/RLS boundary test, using only a disposable CI
// database. It does not provision a Neon branch or imply Console acceptance.
func TestPostgRESTRowSecurityIntegration(t *testing.T) {
	binary := os.Getenv("NEON_DATA_API_TEST_POSTGREST")
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	if binary == "" || dsn == "" {
		t.Fatal("Pinned PostgREST and disposable PostgreSQL are mandatory for full CI")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Path != "/control_ci" && parsed.Path != "/dataapi_ci") {
		t.Fatal("Data API integration requires control_ci or dataapi_ci, never a live metadata DB")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("Disposable CI database unavailable")
	}
	defer conn.Close(context.Background())
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(random[:])
	schema, guard := "dataapi_"+suffix, "dataapi_guard_"+suffix
	auth, web, anon := "dataapi_login_"+suffix, "dataapi_user_"+suffix, "dataapi_anon_"+suffix
	quote := func(name string) string { return pgx.Identifier{name}.Sanitize() }
	password := suffix + suffix // disposable CI credential, never a production input
	setup := []string{
		"CREATE ROLE " + quote(auth) + " LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '" + password + "'",
		"CREATE ROLE " + quote(web) + " NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
		"CREATE ROLE " + quote(anon) + " NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
		"GRANT " + quote(web) + ", " + quote(anon) + " TO " + quote(auth) + " WITH INHERIT FALSE, SET TRUE",
		"CREATE SCHEMA " + quote(schema), "CREATE SCHEMA " + quote(guard),
		"CREATE TABLE " + quote(schema) + ".notes(id text PRIMARY KEY,owner text NOT NULL,body text NOT NULL)",
		"INSERT INTO " + quote(schema) + ".notes VALUES ('a','user-alice','alice-only'),('b','user-bob','bob-only')",
		"ALTER TABLE " + quote(schema) + ".notes ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + quote(schema) + ".notes FORCE ROW LEVEL SECURITY",
		"CREATE POLICY per_subject ON " + quote(schema) + ".notes USING (owner = current_setting('request.jwt.claims',true)::jsonb->>'sub') WITH CHECK (owner = current_setting('request.jwt.claims',true)::jsonb->>'sub')",
		"GRANT USAGE ON SCHEMA " + quote(schema) + ", " + quote(guard) + " TO " + quote(auth) + ", " + quote(web) + ", " + quote(anon),
		"GRANT SELECT,INSERT,UPDATE,DELETE ON " + quote(schema) + ".notes TO " + quote(web),
		fmt.Sprintf(`CREATE FUNCTION %s.enforce_context() RETURNS void LANGUAGE plpgsql SECURITY INVOKER SET search_path=pg_catalog AS $$
DECLARE claims jsonb := current_setting('request.jwt.claims',true)::jsonb;
BEGIN
  IF claims->>'iss' IS DISTINCT FROM 'urn:neon:selfhost:data-api' OR
     claims->>'aud' IS DISTINCT FROM 'urn:neon:branch:%s' OR
     claims->>'branch_id' IS DISTINCT FROM '%s' OR
     COALESCE(claims->>'sub','')='' THEN
    RAISE insufficient_privilege USING MESSAGE='Invalid Data API context';
  END IF;
END $$`, quote(guard), testBranch, testBranch),
		"REVOKE ALL ON FUNCTION " + quote(guard) + ".enforce_context() FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION " + quote(guard) + ".enforce_context() TO " + quote(web) + ", " + quote(anon),
	}
	for i, sql := range setup {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("CI schema setup step %d failed", i)
		}
	}
	// The disposable roles cannot bypass RLS and do not own the exposed table.
	var unsafe int
	err = conn.QueryRow(ctx, `SELECT count(*) FROM pg_roles r WHERE rolname = ANY($1::text[]) AND
 (rolsuper OR rolbypassrls OR rolcreaterole OR rolcreatedb OR rolreplication OR
 oid=(SELECT relowner FROM pg_class WHERE oid=$2::regclass))`, []string{auth, web, anon}, schema+".notes").Scan(&unsafe)
	if err != nil || unsafe != 0 {
		t.Fatal("Data API role privileges are unsafe")
	}
	f := makeFixture(t, "http://127.0.0.1:1")
	f.g.now = time.Now
	f.config.Routes[0].DefaultRole = web
	f.config.Routes[0].AllowedRoles = []string{web, anon}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	f.config.Routes[0].Upstream = "http://127.0.0.1:" + strconv.Itoa(port)
	g, err := New(f.config, f.g.signer.Seed(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	parsed.User = url.UserPassword(auth, password)
	temp := t.TempDir()
	keyPath := filepath.Join(temp, "delegation-public.json")
	if err := os.WriteFile(keyPath, g.DelegationJWKS(), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(temp, "postgrest.conf")
	config := fmt.Sprintf(`db-uri = %s
db-schemas = %s
db-config = false
db-pre-request = %s
db-pool = 1
db-pool-max-idletime = 1
db-max-rows = 100
jwt-secret = %s
jwt-aud = %s
jwt-role-claim-key = "$$.role"
jwt-cache-max-entries = 0
server-host = "127.0.0.1"
server-port = %d
`, strconv.Quote(parsed.String()), strconv.Quote(schema), strconv.Quote(guard+".enforce_context"),
		strconv.Quote("@"+keyPath), strconv.Quote(f.config.Routes[0].Audience), port)
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	process := exec.CommandContext(ctx, binary, configPath)
	process.Stdout, process.Stderr = io.Discard, io.Discard
	// No inherited provider keys, database configuration overrides or proxy
	// environment may alter the pinned child process's test configuration.
	process.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"}
	if err := process.Start(); err != nil {
		t.Fatal("Pinned PostgREST process failed to start")
	}
	defer func() { process.Process.Kill(); process.Wait() }()
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	ready := false
	for attempt := 0; attempt < 60; attempt++ {
		response, err := client.Get(f.config.Routes[0].Upstream + "/")
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == 401 {
				ready = true
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !ready {
		t.Fatal("PostgREST did not become ready with anonymous access disabled")
	}
	app := httptest.NewServer(g)
	defer app.Close()
	query := func(method, path, subject, role, body string) (int, []byte) {
		t.Helper()
		claims := f.claims()
		claims["sub"], claims["role"] = subject, role
		request, err := http.NewRequest(method, app.URL+"/data/v1/"+testBranch+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+f.token(t, claims))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Prefer", "return=representation")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("Data API HTTP request failed")
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 65537))
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, data
	}
	t.Run("Alice and Bob see only their rows", func(t *testing.T) {
		for _, subject := range []string{"user-alice", "user-bob"} {
			status, data := query("GET", "/notes?select=id,owner", subject, web, "")
			var rows []map[string]any
			if status != 200 || json.Unmarshal(data, &rows) != nil || len(rows) != 1 || rows[0]["owner"] != subject {
				t.Fatalf("RLS read failed: status %d", status)
			}
		}
	})
	t.Run("own insert succeeds", func(t *testing.T) {
		status, _ := query("POST", "/notes", "user-alice", web, `{"id":"a2","owner":"user-alice","body":"owned"}`)
		if status != 201 {
			t.Fatalf("owned insert failed: %d", status)
		}
	})
	t.Run("forged ownership denied", func(t *testing.T) {
		status, _ := query("POST", "/notes", "user-alice", web, `{"id":"bad","owner":"user-bob","body":"forged"}`)
		if status != 403 {
			t.Fatalf("RLS write was not denied: %d", status)
		}
	})
	t.Run("other row update invisible", func(t *testing.T) {
		status, data := query("PATCH", "/notes?id=eq.b", "user-alice", web, `{"body":"stolen"}`)
		if status != 200 || strings.TrimSpace(string(data)) != "[]" {
			t.Fatal("foreign row update escaped RLS")
		}
		status, data = query("GET", "/notes?id=eq.b&select=body", "user-bob", web, "")
		if status != 200 || !strings.Contains(string(data), "bob-only") {
			t.Fatal("foreign data changed")
		}
	})
	t.Run("anonymous role has no table privileges", func(t *testing.T) {
		status, _ := query("GET", "/notes", "guest", anon, "")
		if status != 403 {
			t.Fatalf("anonymous table access was not denied: %d", status)
		}
	})
	t.Run("Console and missing bearer cannot authorize", func(t *testing.T) {
		request, _ := http.NewRequest("GET", app.URL+"/data/v1/"+testBranch+"/notes", nil)
		request.Header.Set("Cookie", "neon_v2_session=console-only")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 401 {
			t.Fatal("Console cookie authorized application data")
		}
	})
	// No roles/schemas are dropped during this test: the dedicated disposable
	// CI database preserves the test's database state for the remainder of the job.
}
