package control

import (
	"bytes"
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

const budgetPath = "/api/v1/projects/prj_provider/branches/br_provider/ai-gateway/budget"

func budgetFixture(t *testing.T) *providerFixture {
	f := newProviderFixture(t)
	f.s.ai.Routes[0].Price = &aigateway.Price{Version: "price-original", Currency: "USD", MaxInputTokens: 10, InputRate: 1000000, OutputRate: 2000000}
	f.call("provider_operator", "PUT", budgetPath, "budget-create", record{"version": 0, "enabled": true, "limit_micros": 26}, 200)
	return f
}
func budgetReserve(t *testing.T, f *providerFixture, id string) error {
	t.Helper()
	ok, err := f.s.reserveAIRequest(f.ctx, id, "prj_provider", "br_provider", "provider_operator", "", aigateway.ChatRequest{Model: "static-route", MaxTokens: 8})
	if err == nil && !ok {
		t.Fatal("unexpected concurrency rejection")
	}
	return err
}
func budgetCounters(t *testing.T, f *providerFixture, spent, held int64) {
	t.Helper()
	var a, b int64
	if e := f.s.db.QueryRow(f.ctx, `SELECT spent_micros,reserved_micros FROM ai_budget_policies WHERE branch_id='br_provider'`).Scan(&a, &b); e != nil || a != spent || b != held {
		t.Fatalf("budget counters %d/%d expected %d/%d %v", a, b, spent, held, e)
	}
}
func TestAIBudgetPinnedSettlementAndPolicy(t *testing.T) {
	f := budgetFixture(t)
	id := newID("air_")
	if err := budgetReserve(t, f, id); err != nil {
		t.Fatal(err)
	}
	budgetCounters(t, f, 0, 26)
	changed := *f.s.ai.Routes[0].Price
	changed.Version = "price-later"
	changed.OutputRate = 100000000
	f.s.ai.Routes[0].Price = &changed
	out := aigateway.Outcome{Status: "completed", Dispatched: true, Usage: &aigateway.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}}
	if err := f.s.settleAIRequest(f.ctx, id, "br_provider", out, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := f.s.settleAIRequest(f.ctx, id, "br_provider", out, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	budgetCounters(t, f, 7, 0)
	f.call("provider_operator", "PUT", budgetPath, "budget-edit", record{"version": 1, "enabled": true, "limit_micros": 27}, 200)
	budgetCounters(t, f, 7, 0)
	if err := budgetReserve(t, f, newID("air_")); !errors.Is(err, errAIBudgetExceeded) {
		t.Fatal("budget overspend accepted", err)
	}
	f.call("provider_operator", "PUT", budgetPath, "budget-stale", record{"version": 1, "enabled": true, "limit_micros": 100}, 409)
	f.exec(`UPDATE organization_members SET role='editor' WHERE user_id='provider_org_admin'`)
	f.call("provider_org_admin", "PUT", budgetPath, "editor-budget", record{"version": 2, "enabled": false, "limit_micros": 100}, 403)
	if _, err := f.s.db.Exec(f.ctx, `UPDATE ai_budget_ledger SET spent_delta=0`); err == nil {
		t.Fatal("ledger history mutable")
	}
	var count int
	if f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM ai_budget_ledger WHERE request_id=$1`, id).Scan(&count) != nil || count != 2 {
		t.Fatal("settlement replay created a second ledger event")
	}
}
func TestAIBudgetUncertainNeverExpiresAndReconciles(t *testing.T) {
	f := budgetFixture(t)
	id := newID("air_")
	if err := budgetReserve(t, f, id); err != nil {
		t.Fatal(err)
	}
	if err := f.s.settleAIRequest(f.ctx, id, "br_provider", aigateway.Outcome{Status: "cancelled", Dispatched: true}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE ai_gateway_requests SET expires_at=now()-interval '1 hour' WHERE id=$1`, id)
	if err := budgetReserve(t, f, newID("air_")); !errors.Is(err, errAIBudgetExceeded) {
		t.Fatal("unknown accepted request was refunded by expiry", err)
	}
	budgetCounters(t, f, 0, 26)
	path := "/api/v1/projects/prj_provider/branches/br_provider/ai-gateway/requests/" + id + "/reconcile"
	input := record{"usage": aigateway.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}, "confirmed_not_accepted": false, "evidence_sha256": strings.Repeat("a", 64)}
	f.call("provider_operator", "POST", path, "missing-confirmed-usage", record{"usage": record{}, "confirmed_not_accepted": false, "evidence_sha256": strings.Repeat("a", 64)}, 422)
	f.call("provider_org_admin", "POST", path, "ordinary-admin-reconcile", input, 403)
	f.call("provider_operator", "POST", path, "operator-reconcile", input, 200)
	budgetCounters(t, f, 7, 0)
	w := f.request("provider_operator", "POST", path, "operator-reconcile", input)
	if w.Code != 200 || w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("reconciliation not idempotent")
	}
	f.call("provider_operator", "POST", path, "second-reconcile", input, 409)
	f.call("provider_operator", "POST", path, "operator-reconcile", record{"confirmed_not_accepted": true, "evidence_sha256": strings.Repeat("b", 64)}, 409)
	if err := f.s.settleAIRequest(f.ctx, id, "br_provider", aigateway.Outcome{Status: "completed", Dispatched: true, Usage: &aigateway.Usage{PromptTokens: 9, CompletionTokens: 9, TotalTokens: 18}}, time.Second); err != nil {
		t.Fatal(err)
	}
	budgetCounters(t, f, 7, 0)
	var tokens int64
	if f.s.db.QueryRow(f.ctx, `SELECT total_tokens FROM ai_gateway_requests WHERE id=$1`, id).Scan(&tokens) != nil || tokens != 5 {
		t.Fatal("late transport settlement overwrote confirmed reconciliation")
	}
}
func TestAIBudgetPreDispatchReleaseAndUnknownTariff(t *testing.T) {
	f := budgetFixture(t)
	f.call("provider_operator", "PUT", budgetPath, "missing-policy-enabled", record{"version": 1, "limit_micros": 26}, 422)
	id := newID("air_")
	if err := budgetReserve(t, f, id); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/projects/prj_provider/branches/br_provider/ai-gateway/requests/" + id + "/reconcile"
	f.call("provider_operator", "POST", path, "premature-reconcile", record{"confirmed_not_accepted": true, "evidence_sha256": strings.Repeat("a", 64)}, 409)
	if err := f.s.settleAIRequest(f.ctx, id, "br_provider", aigateway.Outcome{Status: "failed", Dispatched: false}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	budgetCounters(t, f, 0, 0)
	f.s.ai.Routes[0].Price = nil
	if err := budgetReserve(t, f, newID("air_")); !errors.Is(err, errAIBudgetPrice) {
		t.Fatal("unpriced model admitted against a monetary cap", err)
	}
	var count int
	if f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM ai_gateway_requests`).Scan(&count) != nil || count != 1 {
		t.Fatal("failed budget admission committed an inference receipt")
	}
	f.call("provider_operator", "PUT", budgetPath, "budget-off", record{"version": 1, "enabled": false, "limit_micros": 26}, 200)
	if err := budgetReserve(t, f, newID("air_")); err != nil {
		t.Fatal("explicit administrator disable not applied", err)
	}
	budgetCounters(t, f, 0, 0)
}
func TestAIBudgetProviderOverrunChargesTruth(t *testing.T) {
	f := budgetFixture(t)
	id := newID("air_")
	if err := budgetReserve(t, f, id); err != nil {
		t.Fatal(err)
	}
	if err := f.s.settleAIRequest(f.ctx, id, "br_provider", aigateway.Outcome{Status: "completed", Dispatched: true, Usage: &aigateway.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	budgetCounters(t, f, 140, 0)
	if err := budgetReserve(t, f, newID("air_")); !errors.Is(err, errAIBudgetExceeded) {
		t.Fatal("provider overrun did not block further admissions", err)
	}
}

func TestAIBudgetRestrictedGatewayRole(t *testing.T) {
	f := budgetFixture(t)
	var schema string
	if err := f.s.db.QueryRow(f.ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	role := newID("budget_ci_")
	f.exec("CREATE ROLE " + role + " LOGIN PASSWORD 'budget-ci-disposable'")
	f.exec("GRANT USAGE ON SCHEMA " + schema + " TO " + role)
	f.exec("GRANT SELECT ON ai_provider_catalog_state,ai_gateway_requests,ai_budget_policies,ai_budget_reservations TO " + role)
	f.exec("GRANT INSERT,UPDATE ON ai_gateway_requests TO " + role)
	f.exec("GRANT INSERT ON ai_budget_reservations TO " + role)
	f.exec("GRANT UPDATE(state,actual_micros,usage,settled_at) ON ai_budget_reservations TO " + role)
	f.exec("GRANT UPDATE(reserved_micros,spent_micros) ON ai_budget_policies TO " + role)
	f.exec("GRANT INSERT ON ai_budget_ledger TO " + role)
	f.exec("GRANT SELECT ON ai_credit_accounts,ai_credit_reservations TO " + role)
	f.exec("GRANT SELECT(id,project_id) ON branches TO " + role)
	f.exec("GRANT SELECT(id,org_id) ON projects TO " + role)
	f.exec("GRANT UPDATE(spent_micros,reserved_micros) ON ai_credit_accounts TO " + role)
	f.exec("GRANT UPDATE(state,actual_micros,usage,settled_at) ON ai_credit_reservations TO " + role)
	cfg, err := pgxpool.ParseConfig(os.Getenv("NEON_V2_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User = role
	cfg.ConnConfig.Password = "budget-ci-disposable"
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	limited := &server{db: db, ai: f.s.ai, aiProviders: f.s.aiProviders, backendKeys: f.s.backendKeys}
	id := newID("air_")
	ok, err := limited.reserveAIRequest(f.ctx, id, "prj_provider", "br_provider", "provider_operator", "", aigateway.ChatRequest{Model: "static-route", MaxTokens: 8})
	if err != nil || !ok {
		t.Fatal("restricted gateway admission failed", err)
	}
	if err = limited.settleAIRequest(f.ctx, id, "br_provider", aigateway.Outcome{Status: "completed", Dispatched: true, Usage: &aigateway.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}}, time.Millisecond); err != nil {
		t.Fatal("restricted settlement failed", err)
	}
	budgetCounters(t, f, 7, 0)
	for _, sql := range []string{`UPDATE ai_budget_policies SET enabled=false`, `UPDATE ai_budget_policies SET limit_micros=1000000`, `UPDATE ai_budget_reservations SET price='{}'`, `DELETE FROM ai_budget_ledger`, `SELECT response FROM ai_budget_mutations`, `SELECT credential_box FROM ai_provider_revisions`} {
		if _, err = db.Exec(f.ctx, sql); err == nil {
			t.Fatalf("Gateway gained unrelated write/read privilege: %s", sql)
		}
	}
}

func TestAIProviderPublishedTariffAndNativeCapabilities(t *testing.T) {
	f := newProviderFixture(t)
	actor := "provider_operator"
	base := "/api/v1/platform/ai-providers"
	created := f.call(actor, "POST", base, "priced-provider-create", f.draft(0, "sealed", providerTestKey), 202)
	f.work()
	price := &aigateway.Price{Version: "published-v1", Currency: "USD", MaxInputTokens: 10, InputRate: 1000000, OutputRate: 2000000}
	models := providerTestModels("provider-small")
	models[0].Price = price
	models[0].Protocols = []string{"chat_completions", aigateway.Responses, aigateway.Embeddings}
	f.call(actor, "POST", base+"/"+stringVal(created["provider_id"])+"/publish", "priced-provider-publish", aiProviderPublishInput{Version: 1, RevisionID: stringVal(created["revision_id"]), Models: models}, 200)
	price.OutputRate = 9000000 // Publishing pins an immutable database snapshot.
	snapshot, err := f.s.aiCatalog(f.ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	route, ok := snapshot.Config.Find("provider-small")
	if !ok || route.Price == nil || route.Price.OutputRate != 2000000 || !route.Supports(aigateway.Responses) || !route.Supports(aigateway.Embeddings) {
		t.Fatal("published price/capabilities not preserved")
	}
	f.call(actor, "PUT", budgetPath, "priced-provider-budget", record{"version": 0, "enabled": true, "limit_micros": 100}, 200)
	f.invoke("", 200)
	budgetCounters(t, f, 7, 0)
	var reserved, actual int64
	var version, state string
	if err = f.s.db.QueryRow(f.ctx, `SELECT b.reserved_micros,b.actual_micros,b.price->>'version',b.state FROM ai_budget_reservations b JOIN ai_gateway_requests a ON a.id=b.request_id WHERE a.model_id='provider-small'`).Scan(&reserved, &actual, &version, &state); err != nil || reserved != 42 || actual != 7 || version != "published-v1" || state != "settled" {
		t.Fatal("published provider did not use its admitted tariff", err)
	}
}

func TestAIBudgetActualClientCancellationRemainsHeld(t *testing.T) {
	f := budgetFixture(t)
	f.s.aiProviders = nil
	accepted := make(chan struct{})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		http.NewResponseController(w).Flush()
		close(accepted)
		<-r.Context().Done()
	}))
	defer peer.Close()
	f.s.ai.Routes[0].BaseURL = peer.URL + "/v1"
	f.s.ai.Routes[0].Protocols = []string{aigateway.Responses}
	body := []byte(`{"model":"static-route","input":"cancel-after-acceptance","max_output_tokens":8,"stream":true}`)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	r := httptest.NewRequest("POST", "/ai/v1/br_provider/v1/responses", bytes.NewReader(body)).WithContext(ctx)
	r.SetPathValue("branch", "br_provider")
	r.Header.Set("X-Neon-Console-Ticket", f.s.signAIProtocolTicket("provider_operator", "br_provider", "static-route", body, f.s.ai.Generation, aigateway.Responses))
	w := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() { defer close(finished); f.s.aiPublic(w, r) }()
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("provider never accepted the request")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("independent settlement did not finish")
	}
	budgetCounters(t, f, 0, 26)
	var reservation, transport string
	var total *int64
	if err := f.s.db.QueryRow(f.ctx, `SELECT b.state,a.state,a.total_tokens FROM ai_budget_reservations b JOIN ai_gateway_requests a ON a.id=b.request_id WHERE b.request_id=$1`, w.Header().Get("X-Request-ID")).Scan(&reservation, &transport, &total); err != nil || reservation != "uncertain" || transport == "completed" || total != nil {
		t.Fatal("actual cancelled accepted call lost its held budget", err)
	}
}
