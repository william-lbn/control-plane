package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

const organizationProviderPath = "/api/v1/organizations/org_provider/ai-providers"

func organizationProvider(t *testing.T, f *providerFixture, alias string) (string, string) {
	t.Helper()
	f.s.aiProviders.policy.AllowOrganizationBYOK = true
	draft := f.call("provider_org_admin", "POST", organizationProviderPath, "organization-create-"+alias, f.draft(0, "sealed", providerTestKey), 202)
	id, revision := stringVal(draft["provider_id"]), stringVal(draft["revision_id"])
	f.work()
	models := providerTestModels(alias)
	models[0].Price = &aigateway.Price{Version: "byok-price-v1", Currency: "USD", MaxInputTokens: 10, InputRate: 1000000, OutputRate: 2000000}
	f.call("provider_org_admin", "POST", organizationProviderPath+"/"+id+"/publish", "organization-publish-"+alias, aiProviderPublishInput{Version: 1, RevisionID: revision, Models: models}, 200)
	return id, revision
}

func TestAIOrganizationProviderManagementAndIsolation(t *testing.T) {
	f := newProviderFixture(t)
	f.call("provider_org_admin", "GET", organizationProviderPath+"/capabilities", "", nil, 503)
	id, _ := organizationProvider(t, f, "org-private-model")
	caps := f.call("provider_org_admin", "GET", organizationProviderPath+"/capabilities", "", nil, 200)
	if len(caps["credential_sources"].([]any)) != 0 {
		t.Fatal("platform sources exposed to organization")
	}
	f.call("provider_org_admin", "POST", "/api/v1/platform/ai-providers", "org-is-not-operator", f.draft(0, "sealed", providerTestKey), 403)
	listing := f.call("provider_operator", "GET", "/api/v1/platform/ai-providers", "", nil, 200)
	if len(listing["items"].([]any)) != 0 {
		t.Fatal("organization provider leaked into platform management")
	}
	f.exec(`INSERT INTO organizations(id,slug,name) VALUES('org_byok_other','byok-other','Other')`)
	f.exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_byok_other','provider_org_admin','admin')`)
	other := "/api/v1/organizations/org_byok_other/ai-providers"
	f.call("provider_org_admin", "GET", other+"/"+id, "", nil, 404)
	f.call("provider_org_admin", "POST", other+"/"+id+"/revisions", "cross-org-new-revision", f.draft(2, "sealed", providerTestKey), 404)
	f.call("provider_org_admin", "POST", other+"/"+id+"/disable", "cross-org-disable-provider", aiProviderVersionInput{Version: 2}, 404)
	detail := f.call("provider_org_admin", "GET", organizationProviderPath+"/"+id, "", nil, 200)
	operation := detail["revisions"].([]any)[0].(map[string]any)["operation_id"].(string)
	f.call("provider_org_admin", "GET", "/api/v1/organizations/org_byok_other/ai-provider-operations/"+operation, "", nil, 404)
	for _, mode := range []string{"none", "operator_reference"} {
		f.call("provider_org_admin", "POST", organizationProviderPath, "org-cannot-borrow-"+mode, f.draft(0, mode, ""), 422)
	}
	// Being an administrator of both organizations does not confer key ownership.
	reuse := f.draft(0, "reuse", "")
	reuse.Credential.ReuseRevisionID = stringVal(detail["provider"].(map[string]any)["target_revision_id"])
	f.call("provider_org_admin", "POST", other, "cross-provider-credential-reuse", reuse, 422)
	public, err := f.s.aiCatalog(f.ctx, true)
	if err != nil || len(public.Config.Routes) != 1 {
		t.Fatal("organization routes entered unscoped catalog", err)
	}
	own, err := f.s.aiCatalogForOrganization(f.ctx, true, "org_provider")
	if err != nil || len(own.Config.Routes) != 2 {
		t.Fatal("organization catalog missing its own route", err)
	}
	foreign, err := f.s.aiCatalogForOrganization(f.ctx, true, "org_byok_other")
	if err != nil || len(foreign.Config.Routes) != 1 {
		t.Fatal("foreign catalog exposes private model", err)
	}
	f.call("provider_org_admin", "POST", organizationProviderPath+"/"+id+"/disable", "organization-disable-provider", aiProviderVersionInput{Version: 2}, 200)
	f.call("provider_org_admin", "GET", organizationProviderPath+"/"+id, "", nil, 200)
	f.call("provider_org_admin", "DELETE", organizationProviderPath+"/"+id, "organization-delete-provider", aiProviderVersionInput{Version: 3}, 200)
	var revisions, publications int
	if err = f.s.db.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM ai_provider_revisions WHERE provider_id=$1),(SELECT count(*) FROM ai_provider_publications WHERE provider_id=$1)`, id).Scan(&revisions, &publications); err != nil || revisions != 1 || publications != 1 {
		t.Fatal("delete removed credential/publication history", err)
	}
}

func TestAIOrganizationProviderBillingAndImmutableAdmission(t *testing.T) {
	f := newProviderFixture(t)
	id, revision := organizationProvider(t, f, "org-billed-model")
	f.call("provider_operator", "PUT", creditPlatformPath, "byok-empty-platform-account", record{"version": 0, "enabled": true}, 200)
	f.call("provider_operator", "PUT", budgetPath, "byok-still-bounded", record{"version": 0, "enabled": true, "limit_micros": 1000}, 200)
	snapshot, err := f.s.aiBranchCatalog(f.ctx, true, "br_provider")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"org-billed-model","messages":[{"role":"user","content":"local BYOK functional request"}],"max_tokens":8}`)
	ticket := f.s.signAITicketFor("provider_org_admin", "br_provider", "org-billed-model", body, snapshot.Config.Generation)
	r := httptest.NewRequest("POST", "/ai/v1/br_provider/v1/chat/completions", bytes.NewReader(body))
	r.SetPathValue("branch", "br_provider")
	r.Header.Set("X-Neon-Console-Ticket", ticket)
	w := httptest.NewRecorder()
	f.s.aiPublic(w, r)
	if w.Code != 200 || f.posts.Load() != 1 {
		t.Fatalf("BYOK did not reach its own supplier: %d %s", w.Code, w.Body.String())
	}
	creditCounters(t, f, 0, 0, 0)
	var spent, held int64
	if err = f.s.db.QueryRow(f.ctx, `SELECT spent_micros,reserved_micros FROM ai_budget_policies WHERE branch_id='br_provider'`).Scan(&spent, &held); err != nil || spent != 7 || held != 0 {
		t.Fatal("BYOK escaped branch budget", spent, held, err)
	}
	var owner, org, pid, rid string
	request := w.Header().Get("X-Request-ID")
	if err = f.s.db.QueryRow(f.ctx, `SELECT billing_owner,billing_org_id,provider_id,provider_revision_id FROM ai_gateway_requests WHERE id=$1`, request).Scan(&owner, &org, &pid, &rid); err != nil || owner != "organization_byok" || org != "org_provider" || pid != id || rid != revision {
		t.Fatal("request lost immutable supplier billing attribution", err)
	}
	if _, err = f.s.db.Exec(f.ctx, `UPDATE ai_gateway_requests SET billing_owner='platform',billing_org_id=NULL WHERE id=$1`, request); err == nil {
		t.Fatal("billing attribution mutable")
	}
	if _, err = f.s.db.Exec(f.ctx, `UPDATE ai_providers SET org_id=NULL WHERE id=$1`, id); err == nil {
		t.Fatal("provider ownership mutable")
	}
	forged := *snapshot.Config
	forged.Routes = append([]aigateway.Route(nil), snapshot.Config.Routes...)
	for i := range forged.Routes {
		if forged.Routes[i].ID == "org-billed-model" {
			forged.Routes[i].OwnerOrgID = ""
		}
	}
	if _, err = f.s.reserveAIRequestFor(f.ctx, newID("air_"), "prj_provider", "br_provider", "provider_org_admin", "", aigateway.ChatRequest{Model: "org-billed-model", MaxTokens: 8}, &forged); err == nil {
		t.Fatal("forged platform attribution bypassed persisted provider ownership")
	}
	// An accepted unknown BYOK result retains the branch budget, never platform funds.
	unknown := newID("air_")
	if ok, e := f.s.reserveAIRequestFor(f.ctx, unknown, "prj_provider", "br_provider", "provider_org_admin", "", aigateway.ChatRequest{Model: "org-billed-model", MaxTokens: 8}, snapshot.Config); e != nil || !ok {
		t.Fatal(e)
	}
	if err = f.s.settleAIRequest(f.ctx, unknown, "br_provider", aigateway.Outcome{Status: "usage_unknown", Dispatched: true}, 0); err != nil {
		t.Fatal(err)
	}
	creditCounters(t, f, 0, 0, 0)
	if err = f.s.db.QueryRow(f.ctx, `SELECT reserved_micros FROM ai_budget_policies WHERE branch_id='br_provider'`).Scan(&held); err != nil || held != 26 {
		t.Fatal("unknown BYOK budget was refunded", err)
	}
}

func TestAIOrganizationProviderRevokedAuthorityAndScope(t *testing.T) {
	f := newProviderFixture(t)
	f.s.aiProviders.policy.AllowOrganizationBYOK = true
	draft := f.call("provider_org_admin", "POST", organizationProviderPath, "revoked-org-provider", f.draft(0, "sealed", providerTestKey), 202)
	f.exec(`UPDATE organization_members SET role='editor' WHERE user_id='provider_org_admin'`)
	f.work()
	if f.gets.Load() != 0 {
		t.Fatal("revoked administrator triggered supplier discovery")
	}
	f.call("provider_org_admin", "GET", organizationProviderPath, "", nil, 403)
	var state, code string
	if err := f.s.db.QueryRow(f.ctx, `SELECT state,error_code FROM ai_provider_operations WHERE id=$1`, draft["operation_id"]).Scan(&state, &code); err != nil || state != "failed" || code != "provider_operation_superseded" {
		t.Fatal("revoked operation was not fenced", state, code, err)
	}
	f.exec(`UPDATE organization_members SET role='admin' WHERE user_id='provider_org_admin'`)
	organizationProvider(t, f, "org-state-model")
	f.exec(`UPDATE organizations SET state='suspended' WHERE id='org_provider'`)
	snapshot, err := f.s.aiCatalogForOrganization(f.ctx, true, "org_provider")
	if err != nil || len(snapshot.Config.Routes) != 1 {
		t.Fatal("inactive organization retained a published route", err)
	}
}

func TestAIOrganizationProviderRestrictedGatewayRole(t *testing.T) {
	f := newProviderFixture(t)
	organizationProvider(t, f, "org-limited-model")
	var schema string
	if err := f.s.db.QueryRow(f.ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	role := newID("byokread_")
	f.exec("CREATE ROLE " + role + " LOGIN PASSWORD 'disposable-byok-read'")
	f.exec("GRANT USAGE ON SCHEMA " + schema + " TO " + role)
	f.exec("GRANT SELECT ON ai_provider_catalog_state,ai_provider_runtime,ai_gateway_requests,ai_budget_policies,ai_credit_accounts,ai_credit_reservations TO " + role)
	f.exec("GRANT SELECT(id,project_id) ON branches TO " + role)
	f.exec("GRANT SELECT(id,org_id) ON projects TO " + role)
	f.exec("GRANT INSERT ON ai_gateway_requests TO " + role)
	f.exec("GRANT UPDATE(reserved_micros) ON ai_budget_policies TO " + role)
	cfg, err := pgxpool.ParseConfig(os.Getenv("NEON_V2_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User = role
	cfg.ConnConfig.Password = "disposable-byok-read"
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	limited := &server{db: db, ai: f.s.ai, aiProviders: f.s.aiProviders}
	snapshot, err := limited.aiBranchCatalog(f.ctx, true, "br_provider")
	if err != nil {
		t.Fatal(err)
	}
	if ok, e := limited.reserveAIRequestFor(f.ctx, newID("air_"), "prj_provider", "br_provider", "provider_org_admin", "", aigateway.ChatRequest{Model: "org-limited-model", MaxTokens: 8}, snapshot.Config); e != nil || !ok {
		t.Fatal("Gateway required unpublished provider-table rights", e)
	}
	if _, err = db.Exec(f.ctx, `SELECT org_id FROM ai_providers`); err == nil {
		t.Fatal("Gateway can read base provider ownership")
	}
	// Public metadata identifies fee ownership without serializing private scope or keys.
	raw, _ := json.Marshal(snapshot.Config.PublicModels())
	if bytes.Contains(raw, []byte("org_provider")) || bytes.Contains(raw, []byte(providerTestKey)) || !bytes.Contains(raw, []byte("organization_byok")) {
		t.Fatal("public model billing fields leaked private scope or omitted fee ownership")
	}
}

func TestAIOrganizationProviderAliasAndSharedCapacity(t *testing.T) {
	f := newProviderFixture(t)
	// Exactly 98 static + 1 private + 1 shared aliases is a functional catalog
	// boundary. This does not send inference or perform a throughput campaign.
	template := f.s.ai.Routes[0]
	f.s.ai.Routes = nil
	for i := 0; i < 98; i++ {
		route := template
		route.ID = fmt.Sprintf("static-%02d", i)
		f.s.ai.Routes = append(f.s.ai.Routes, route)
	}
	organizationProvider(t, f, "private-same")
	f.exec(`INSERT INTO organizations(id,slug,name) VALUES('org_byok_second','byok-second','Second')`)
	f.exec(`INSERT INTO organization_members(org_id,user_id,role) VALUES('org_byok_second','provider_org_admin','admin')`)
	other := "/api/v1/organizations/org_byok_second/ai-providers"
	created := f.call("provider_org_admin", "POST", other, "second-org-own-sealed-key", f.draft(0, "sealed", providerTestKey), 202)
	f.work()
	id, revision := stringVal(created["provider_id"]), stringVal(created["revision_id"])
	f.call("provider_org_admin", "POST", other+"/"+id+"/publish", "second-org-independent-alias", aiProviderPublishInput{Version: 1, RevisionID: revision, Models: providerTestModels("private-same")}, 200)
	platform := "/api/v1/platform/ai-providers"
	shared := f.call("provider_operator", "POST", platform, "platform-shared-boundary-draft", f.draft(0, "sealed", providerTestKey), 202)
	f.work()
	pid, rev := stringVal(shared["provider_id"]), stringVal(shared["revision_id"])
	f.call("provider_operator", "POST", platform+"/"+pid+"/publish", "shared-cannot-shadow-private", aiProviderPublishInput{Version: 1, RevisionID: rev, Models: providerTestModels("private-same")}, 409)
	f.call("provider_operator", "POST", platform+"/"+pid+"/publish", "shared-exact-capacity-not-double-counted", aiProviderPublishInput{Version: 1, RevisionID: rev, Models: providerTestModels("shared-model")}, 200)
	for _, org := range []string{"org_provider", "org_byok_second"} {
		snapshot, err := f.s.aiCatalogForOrganization(f.ctx, true, org)
		if err != nil || len(snapshot.Config.Routes) != 100 {
			t.Fatal("scoped exact-capacity catalog unavailable", org, err)
		}
	}
	models := append(providerTestModels("shared-model"), providerTestModels("extra-shared-model")...)
	f.call("provider_operator", "POST", platform+"/"+pid+"/publish", "shared-cannot-overfill-private-catalog", aiProviderPublishInput{Version: 2, RevisionID: rev, Models: models}, 429)
}
