package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestComputeCatalogBootstrapCreationStates(t *testing.T) {
	for _, tc := range []struct {
		action, project, branch, endpoint string
		legacy, want                      bool
	}{
		{"create_project", "provisioning", "creating", "provisioning", false, true},
		{"create_project", "error", "error", "error", false, true},
		{"create_branch", "ready", "creating", "provisioning", false, true},
		{"create_branch", "ready", "error", "error", false, true},
		{"create_endpoint", "ready", "ready", "provisioning", false, true},
		{"create_endpoint", "ready", "ready", "error", false, true},
		{"create_project", "ready", "ready", "active", true, true},
		{"create_endpoint", "ready", "ready", "suspended", true, true},
		{"create_branch", "error", "creating", "provisioning", false, false},
		{"create_endpoint", "ready", "error", "provisioning", false, false},
		{"create_project", "deleting", "ready", "active", false, false},
		{"create_branch", "ready", "deleting", "active", false, false},
		{"create_endpoint", "ready", "ready", "deleting", false, false},
		{"create_project", "provisioning", "creating", "provisioning", true, false},
		{"create_branch", "ready", "error", "error", true, false},
	} {
		if computeCatalogBootstrapStates(tc.action, tc.project, tc.branch, tc.endpoint, tc.legacy) != tc.want {
			t.Fatalf("wrong creation state decision: %+v", tc)
		}
	}
}

func TestComputeCatalogBootstrapWitnessAndFixedBackingPublication(t *testing.T) {
	dsn := os.Getenv("NEON_V2_TEST_DATABASE_URL")
	cfg, err := pgxpool.ParseConfig(dsn)
	if dsn == "" || err != nil || cfg.ConnConfig.Database != "control_ci" {
		t.Fatal("disposable control_ci required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "v2_bootstrap_" + randomToken(6)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgQuote(schema)); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+pgQuote(schema)+" CASCADE")
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	const project = "prj_0123456789abcdef"
	const branch = "br_0123456789abcdef"
	const endpoint = "ep_0123456789abcdef"
	const operation = "op_0123456789abcdef01234567"
	const worker = "bootstrap-worker"
	p := createPayload{ProjectID: project, BranchID: branch, EndpointID: endpoint, TenantID: strings.Repeat("a", 32), TimelineID: strings.Repeat("b", 32), EndpointType: "read_write"}
	raw, _ := json.Marshal(p)
	exec(`INSERT INTO users(id,username,password_hash,role) VALUES('bootstrap_admin','bootstrap_admin','unused','owner')`)
	exec(`INSERT INTO organizations(id,slug,name) VALUES('bootstrap_org','bootstrap-org','Bootstrap')`)
	exec(`INSERT INTO organization_quotas(org_id) VALUES('bootstrap_org')`)
	exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,tenant_id,created_at,updated_at) VALUES($1,'bootstrap_org','Bootstrap','lab',16,'ready','managed',$2,now(),now())`, project, p.TenantID)
	exec(`INSERT INTO branches(id,project_id,name,timeline_id,state,created_at) VALUES($1,$2,'main',$3,'ready',now())`, branch, project, p.TimelineID)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,endpoint_type,created_at,updated_at) VALUES($1,$2,$3,$1,'neonvm',$1,$1,'active','cloud_admin','postgres','read_write',now(),now())`, endpoint, project, branch)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload,lease_owner,lease_expires_at) VALUES($1,$2,'project',$2,'create_project','running','bootstrap_admin','bootstrap-request',$3::jsonb,$4,now()+interval '5 minutes')`, operation, project, raw, worker)
	exec(`INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,0,'sql_probe_via_proxy','running')`, operation)
	encoded, _ := computeConfig(project, p.TenantID, p.TimelineID, "admin-intent", "probe-intent", p.EndpointType)
	var document map[string]any
	_ = json.Unmarshal([]byte(encoded), &document)
	document["untouched"] = map[string]any{"nested": []any{true, "unrelated"}}
	document["spec"].(map[string]any)["delta_operations"] = []any{map[string]any{"action": "delete_role", "name": "retained_tombstone"}}
	encode := func(v any) string { raw, _ := json.Marshal(v); return base64.StdEncoding.EncodeToString(raw) }
	config := map[string]any{"metadata": map[string]any{"uid": "config-original", "resourceVersion": "1", "labels": credentialLabels(project, endpoint)}, "data": map[string]any{"config.json": encode(document), "probePassword": base64.StdEncoding.EncodeToString([]byte("probe-password")), "unrelated": "retained"}}
	credential := map[string]any{"metadata": map[string]any{"uid": "credential-original", "labels": credentialLabels(project, endpoint)}, "data": map[string]any{"adminVerifier": base64.StdEncoding.EncodeToString([]byte("admin-intent")), "probeVerifier": base64.StdEncoding.EncodeToString([]byte("probe-intent")), "probePassword": base64.StdEncoding.EncodeToString([]byte("probe-password"))}}
	clone := func(v map[string]any) map[string]any {
		raw, _ := json.Marshal(v)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return out
	}
	puts := 0
	conflict := false
	lost := false
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "-credentials") {
			jsonResponse(w, 200, credential)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "-config") {
			w.WriteHeader(500)
			return
		}
		if r.Method == http.MethodPut {
			puts++
			var value map[string]any
			_ = json.NewDecoder(r.Body).Decode(&value)
			if nested(value, "metadata", "uid") != nested(config, "metadata", "uid") || nested(value, "metadata", "resourceVersion") != nested(config, "metadata", "resourceVersion") {
				w.WriteHeader(409)
				return
			}
			if conflict {
				conflict = false
				config["metadata"].(map[string]any)["resourceVersion"] = "2"
				config["data"].(map[string]any)["concurrent"] = "retained"
				w.WriteHeader(409)
				return
			}
			config = value
			config["metadata"].(map[string]any)["resourceVersion"] = "3"
			if lost {
				lost = false
				w.WriteHeader(503)
				return
			}
		}
		jsonResponse(w, 200, config)
	}))
	defer peer.Close()
	s := &server{db: db, kube: &kubeClient{base: peer.URL, namespace: "neon", http: peer.Client()}}
	_, _, v, err := s.kube.readComputeCatalogBootstrap(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"wrong_worker", "legacy_unverified", "retiring_parent", "wrong_payload", "wrong_scope", "not_probing", "competing"} {
		t.Run(scenario, func(t *testing.T) {
			candidate := p
			observed := v
			w := worker
			legacy := false
			switch scenario {
			case "wrong_worker":
				w = "stale-worker"
			case "legacy_unverified":
				legacy = true
			case "retiring_parent":
				exec(`UPDATE branches SET state='deleting' WHERE id=$1`, branch)
				defer exec(`UPDATE branches SET state='ready' WHERE id=$1`, branch)
			case "wrong_payload":
				exec(`UPDATE operations SET payload=jsonb_set(payload,'{timeline_id}','"wrong"') WHERE id=$1`, operation)
				defer exec(`UPDATE operations SET payload=$2::jsonb WHERE id=$1`, operation, raw)
			case "wrong_scope":
				observed.Timeline = "wrong"
			case "not_probing":
				exec(`UPDATE operation_steps SET state='failed' WHERE operation_id=$1`, operation)
				defer exec(`UPDATE operation_steps SET state='running' WHERE operation_id=$1`, operation)
			case "competing":
				exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id) VALUES('bootstrap_competing',$1,'branch_catalog',$2,'create_role','queued','bootstrap_admin','competitor')`, project, branch)
				defer exec(`DELETE FROM operations WHERE id='bootstrap_competing'`)
			}
			if _, err := s.storeComputeCatalogBootstrap(ctx, operation, w, candidate, legacy, observed); err == nil {
				t.Fatal("unverified bootstrap accepted")
			}
		})
	}
	if puts != 0 {
		t.Fatal("failed receipt mutated Kubernetes")
	}
	stored, err := s.storeComputeCatalogBootstrap(ctx, operation, worker, p, false, v)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Witness != "original_sql_probe" {
		t.Fatal("original witness missing")
	}
	conflict = true
	lost = true
	if err = s.publishComputeCatalogBootstrap(ctx, stored); err == nil {
		t.Fatal("unknown write outcome was hidden")
	}
	if err = s.resumeComputeCatalogBootstrap(ctx, p); err != nil {
		t.Fatal("original UID readback did not recover committed write", err)
	}
	if puts != 2 || nested(config, "data", "concurrent") != "retained" || nested(config, "data", "unrelated") != "retained" {
		t.Fatal("CAS lost unrelated config")
	}
	text, _ := secretText(config, "config.json")
	var after map[string]any
	_ = json.Unmarshal([]byte(text), &after)
	after["spec"].(map[string]any)["skip_pg_catalog_updates"] = false
	document["spec"].(map[string]any)["skip_pg_catalog_updates"] = false
	beforeRaw, _ := json.Marshal(document)
	afterRaw, _ := json.Marshal(after)
	if string(beforeRaw) != string(afterRaw) {
		t.Fatal("publication modified startup fields other than catalog flag")
	}
	for _, scenario := range []string{"config_uid", "credential_uid", "timeline", "verifier", "optional_service", "duplicate_scope", "flag_type", "missing_probe"} {
		t.Run(scenario, func(t *testing.T) {
			originalConfig := clone(config)
			originalCredential := clone(credential)
			defer func() { config = originalConfig; credential = originalCredential }()
			text, _ := secretText(config, "config.json")
			var changed map[string]any
			_ = json.Unmarshal([]byte(text), &changed)
			spec := changed["spec"].(map[string]any)
			cluster := spec["cluster"].(map[string]any)
			switch scenario {
			case "config_uid":
				config["metadata"].(map[string]any)["uid"] = "successor"
			case "credential_uid":
				credential["metadata"].(map[string]any)["uid"] = "successor"
			case "timeline":
				for _, setting := range cluster["settings"].([]any) {
					item := setting.(map[string]any)
					if item["name"] == "neon.timeline_id" {
						item["value"] = "wrong"
					}
				}
			case "verifier":
				cluster["roles"].([]any)[0].(map[string]any)["encrypted_password"] = "other"
			case "optional_service":
				spec["local_proxy_config"] = map[string]any{}
			case "duplicate_scope":
				cluster["settings"] = append(cluster["settings"].([]any), map[string]any{"name": "neon.timeline_id", "value": p.TimelineID})
			case "flag_type":
				spec["skip_pg_catalog_updates"] = "true"
			case "missing_probe":
				delete(credential["data"].(map[string]any), "probePassword")
			}
			config["data"].(map[string]any)["config.json"] = encode(changed)
			previous := puts
			if err = s.publishComputeCatalogBootstrap(ctx, stored); err == nil {
				t.Fatal("changed backing accepted")
			}
			if puts != previous {
				t.Fatal("changed backing written")
			}
		})
	}
	exec(`UPDATE operations SET state='succeeded' WHERE id=$1`, operation)
	exec(`UPDATE operation_steps SET state='succeeded' WHERE operation_id=$1`, operation)
	if err = s.sealComputeCatalogBootstrap(ctx, operation, "", p, true); err != nil {
		t.Fatal("legacy observation replaced original receipt", err)
	}
	var count int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM compute_catalog_bootstrap_receipts`).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate bootstrap receipt", err)
	}
	for _, table := range []string{"compute_catalog_bootstrap_receipts", "compute_catalog_bootstrap_publications"} {
		if _, err = db.Exec(ctx, "DELETE FROM "+table); err == nil {
			t.Fatal("bootstrap history mutable")
		}
	}
	// A new legacy Endpoint is observed without creating/waking a VM or SQL call.
	// Its old credential has no probeVerifier field. The original protected
	// password must prove the retained config verifier without any Secret write.
	oldVerifier, err := scramVerifier("probe-password")
	if err != nil {
		t.Fatal(err)
	}
	text, _ = secretText(config, "config.json")
	_ = json.Unmarshal([]byte(text), &after)
	for _, entry := range nested(after, "spec", "cluster", "roles").([]any) {
		item := entry.(map[string]any)
		if item["name"] == "control_probe" {
			item["encrypted_password"] = oldVerifier
		}
	}
	config["data"].(map[string]any)["config.json"] = encode(after)
	delete(credential["data"].(map[string]any), "probeVerifier")
	if _, _, _, err = s.kube.readComputeCatalogBootstrap(ctx, p); err != nil {
		t.Fatal("valid original legacy probe rejected", err)
	}
	passwordBefore := credential["data"].(map[string]any)["probePassword"]
	credential["data"].(map[string]any)["probePassword"] = base64.StdEncoding.EncodeToString([]byte("wrong-probe-password"))
	if _, _, _, err = s.kube.readComputeCatalogBootstrap(ctx, p); err == nil {
		t.Fatal("legacy config adopted with wrong protected password")
	}
	credential["data"].(map[string]any)["probePassword"] = passwordBefore
	legacyP := p
	legacyP.EndpointID = "ep_1123456789abcdef"
	legacyP.EndpointType = "read_only"
	legacyOp := "op_1123456789abcdef01234567"
	legacyRaw, _ := json.Marshal(legacyP)
	exec(`INSERT INTO endpoints(id,project_id,branch_id,selector,workload_kind,workload_name,service_name,state,role_name,database_name,endpoint_type,created_at,updated_at) SELECT $1,project_id,branch_id,$1,workload_kind,$1,$1,state,role_name,database_name,'read_only',created_at,updated_at FROM endpoints WHERE id=$2`, legacyP.EndpointID, endpoint)
	exec(`INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload) VALUES($1,$2,'endpoint',$3,'create_endpoint','succeeded','bootstrap_admin','legacy-bootstrap',$4::jsonb)`, legacyOp, project, legacyP.EndpointID, legacyRaw)
	exec(`INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES($1,0,'sql_probe_via_proxy','succeeded')`, legacyOp)
	config["metadata"].(map[string]any)["labels"] = credentialLabels(project, legacyP.EndpointID)
	credential["metadata"].(map[string]any)["labels"] = credentialLabels(project, legacyP.EndpointID)
	text, _ = secretText(config, "config.json")
	_ = json.Unmarshal([]byte(text), &after)
	after["spec"].(map[string]any)["mode"] = "Replica"
	config["data"].(map[string]any)["config.json"] = encode(after)
	if err = s.reconcileLegacyComputeCatalogBootstraps(ctx); err != nil {
		t.Fatal(err)
	}
	var proof computeCatalogBootstrap
	var proofRaw []byte
	if err = db.QueryRow(ctx, `SELECT proof FROM compute_catalog_bootstrap_receipts WHERE endpoint_id=$1`, legacyP.EndpointID).Scan(&proofRaw); err != nil || json.Unmarshal(proofRaw, &proof) != nil || proof.Witness != "legacy_successful_probe_initial_backing_observation" {
		t.Fatal("legacy witness lost initial observation boundary", err)
	}
}
