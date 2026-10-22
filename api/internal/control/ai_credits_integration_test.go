package control

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

const creditPlatformPath = "/api/v1/platform/ai-credits/organizations/org_provider"
const creditOrgPath = "/api/v1/organizations/org_provider/ai-credits"

func creditFixture(t *testing.T, amount int64) *providerFixture {
	f := newProviderFixture(t)
	f.s.ai.Routes[0].Price = &aigateway.Price{Version: "credits-price-v1", Currency: "USD", MaxInputTokens: 10, InputRate: 1000000, OutputRate: 2000000}
	f.call("provider_operator", "PUT", creditPlatformPath, "credits-enable-original", record{"version": 0, "enabled": true}, 200)
	f.call("provider_operator", "POST", creditPlatformPath+"/entries", "credits-original-grant", record{"version": 1, "kind": "grant", "amount_micros": amount, "source_event_id": "credits-original-grant-event", "reason": "Explicit disposable operator credit fixture", "evidence_sha256": strings.Repeat("a", 64)}, 200)
	return f
}

func creditCounters(t *testing.T, f *providerFixture, credited, spent, held int64) {
	t.Helper()
	var a, b, c int64
	err := f.s.db.QueryRow(f.ctx, `SELECT credited_micros,spent_micros,reserved_micros FROM ai_credit_accounts WHERE org_id='org_provider'`).Scan(&a, &b, &c)
	if err != nil || a != credited || b != spent || c != held {
		t.Fatalf("credit counters %d/%d/%d expected %d/%d/%d: %v", a, b, c, credited, spent, held, err)
	}
}

func TestAICreditsOperatorGrantsAndScopedHistory(t *testing.T) {
	f := newProviderFixture(t)
	initial := f.call("provider_org_admin", "GET", creditOrgPath, "", nil, 200)
	if initial["can_manage"] != false || initial["payment_processor"] != false {
		t.Fatal("organization admin gained platform credit authority")
	}
	f.call("provider_org_admin", "PUT", creditPlatformPath, "ordinary-owner-cannot-fund", record{"version": 0, "enabled": true}, 403)
	f.call("provider_operator", "PUT", creditPlatformPath, "missing-enabled-is-not-free", record{"version": 0}, 422)
	f.call("provider_operator", "PUT", creditPlatformPath, "operator-enable-credit", record{"version": 0, "enabled": true}, 200)
	grant := record{"version": 1, "kind": "grant", "amount_micros": int64(100), "source_event_id": "credit-once-event-01", "reason": "Disposable operator manual credit grant", "evidence_sha256": strings.Repeat("a", 64)}
	f.call("provider_org_admin", "POST", creditPlatformPath+"/entries", "owner-cannot-issue-credit", grant, 403)
	f.call("provider_operator", "POST", creditPlatformPath+"/entries", "credit-once-key-01", grant, 200)
	w := f.request("provider_operator", "POST", creditPlatformPath+"/entries", "credit-once-key-01", grant)
	if w.Code != 200 || w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("lost grant was not replayed")
	}
	creditCounters(t, f, 100, 0, 0)
	duplicate := record{"version": 2, "kind": "grant", "amount_micros": int64(100), "source_event_id": "credit-once-event-01", "reason": "Same source from a different submission", "evidence_sha256": strings.Repeat("a", 64)}
	f.call("provider_operator", "POST", creditPlatformPath+"/entries", "different-key-same-source", duplicate, 409)
	grant["amount_micros"] = 101
	f.call("provider_operator", "POST", creditPlatformPath+"/entries", "credit-once-key-01", grant, 409)
	f.call("provider_operator", "POST", creditPlatformPath+"/entries", "explicit-credit-correction", record{"version": 2, "kind": "adjustment", "amount_micros": -10, "source_event_id": "credit-correction-event-01", "reason": "Explicit disposable evidence correction", "evidence_sha256": strings.Repeat("b", 64)}, 200)
	creditCounters(t, f, 90, 0, 0)
	f.call("provider_operator", "PUT", creditPlatformPath, "stale-credit-policy", record{"version": 2, "enabled": false}, 409)
	f.call("provider_operator", "PUT", creditPlatformPath, "credit-policy-disable", record{"version": 3, "enabled": false}, 200)
	creditCounters(t, f, 90, 0, 0)
	if _, err := f.s.db.Exec(f.ctx, `UPDATE ai_credit_ledger SET credit_delta=0`); err == nil {
		t.Fatal("credit ledger mutable")
	}
	if _, err := f.s.db.Exec(f.ctx, `DELETE FROM ai_credit_mutations`); err == nil {
		t.Fatal("credit replay history mutable")
	}
	f.exec(`UPDATE organization_members SET role='editor' WHERE user_id='provider_org_admin'`)
	f.call("provider_org_admin", "GET", creditOrgPath, "", nil, 403)
	f.exec(`INSERT INTO organizations(id,slug,name) VALUES('org_credit_other','credit_other','Other')`)
	f.call("provider_operator", "GET", "/api/v1/organizations/org_credit_other/ai-credits", "", nil, 404)
	f.call("provider_operator", "GET", "/api/v1/platform/ai-credits/organizations/org_credit_other", "", nil, 200)
	if f.posts.Load() != 0 || f.gets.Load() != 0 {
		t.Fatal("credit configuration contacted a supplier")
	}
}

func TestAICreditsDualAccountingPinnedUsageAndAtomicDenial(t *testing.T) {
	f := creditFixture(t, 26)
	f.call("provider_operator", "PUT", budgetPath, "dual-budget-enable", record{"version": 0, "enabled": true, "limit_micros": 1000}, 200)
	id := newID("air_")
	if err := budgetReserve(t, f, id); err != nil {
		t.Fatal(err)
	}
	creditCounters(t, f, 26, 0, 26)
	budgetCounters(t, f, 0, 26)
	changed := *f.s.ai.Routes[0].Price
	changed.Version = "credit-price-changed"
	changed.OutputRate = 100000000
	f.s.ai.Routes[0].Price = &changed
	out := aigateway.Outcome{Status: "completed", Dispatched: true, Usage: &aigateway.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}}
	if err := f.s.settleAIRequest(f.ctx, id, "br_provider", out, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := f.s.settleAIRequest(f.ctx, id, "br_provider", out, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	creditCounters(t, f, 26, 7, 0)
	budgetCounters(t, f, 7, 0)
	// The branch has ample budget, but credits must reject and roll back every
	// earlier branch-budget/request insert in this same admission transaction.
	changed.OutputRate = 2000000
	f.s.ai.Routes[0].Price = &changed
	denied := newID("air_")
	if err := budgetReserve(t, f, denied); !errors.Is(err, errAICreditsExceeded) {
		t.Fatal("organization overdraw admitted", err)
	}
	creditCounters(t, f, 26, 7, 0)
	budgetCounters(t, f, 7, 0)
	var exists bool
	if f.s.db.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM ai_gateway_requests WHERE id=$1)`, denied).Scan(&exists) != nil || exists {
		t.Fatal("denied request was partially committed")
	}
	var count int
	if f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM ai_credit_ledger WHERE request_id=$1`, id).Scan(&count) != nil || count != 2 {
		t.Fatal("settlement charged twice")
	}
}

func TestAICreditsUncertainDeletedBranchRecoveryAndLateResult(t *testing.T) {
	f := creditFixture(t, 26)
	f.call("provider_operator", "PUT", budgetPath, "reconcile-dual-budget", record{"version": 0, "enabled": true, "limit_micros": 1000}, 200)
	id := newID("air_")
	if err := budgetReserve(t, f, id); err != nil {
		t.Fatal(err)
	}
	if err := f.s.settleAIRequest(f.ctx, id, "br_provider", aigateway.Outcome{Status: "cancelled", Dispatched: true}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE ai_gateway_requests SET expires_at=now()-interval '1 hour' WHERE id=$1`, id)
	if err := budgetReserve(t, f, newID("air_")); !errors.Is(err, errAICreditsExceeded) {
		t.Fatal("uncertain credit refunded by expiry", err)
	}
	creditCounters(t, f, 26, 0, 26)
	f.exec(`UPDATE branches SET state='deleted',deleted_at=now() WHERE id='br_provider'`)
	path := "/api/v1/platform/ai-credits/requests/" + id + "/reconcile"
	input := record{"usage": aigateway.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}, "confirmed_not_accepted": false, "evidence_sha256": strings.Repeat("c", 64)}
	f.call("provider_org_admin", "POST", path, "ordinary-admin-cannot-reconcile-credit", input, 403)
	f.call("provider_operator", "POST", path, "missing-credit-token-counts", record{"usage": record{}, "confirmed_not_accepted": false, "evidence_sha256": strings.Repeat("c", 64)}, 422)
	f.call("provider_operator", "POST", path, "original-deleted-credit-reconcile", input, 200)
	w := f.request("provider_operator", "POST", path, "original-deleted-credit-reconcile", input)
	if w.Code != 200 || w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("deleted resource evidence not idempotent")
	}
	creditCounters(t, f, 26, 7, 0)
	budgetCounters(t, f, 7, 0)
	f.call("provider_operator", "POST", path, "credit-no-final-overwrite", input, 409)
	if err := f.s.settleAIRequest(f.ctx, id, "br_provider", aigateway.Outcome{Status: "completed", Dispatched: true, Usage: &aigateway.Usage{PromptTokens: 9, CompletionTokens: 9, TotalTokens: 18}}, time.Second); err != nil {
		t.Fatal(err)
	}
	creditCounters(t, f, 26, 7, 0)
	budgetCounters(t, f, 7, 0)
	var total int64
	if f.s.db.QueryRow(f.ctx, `SELECT total_tokens FROM ai_gateway_requests WHERE id=$1`, id).Scan(&total) != nil || total != 5 {
		t.Fatal("late result replaced confirmed usage")
	}
}

func TestAICreditsPriceRequirementReleaseAndDisabledSettlement(t *testing.T) {
	f := creditFixture(t, 100)
	price := f.s.ai.Routes[0].Price
	f.s.ai.Routes[0].Price = nil
	if err := budgetReserve(t, f, newID("air_")); !errors.Is(err, errAICreditsPrice) {
		t.Fatal("missing tariff was made free", err)
	}
	f.s.ai.Routes[0].Price = price
	id := newID("air_")
	if err := budgetReserve(t, f, id); err != nil {
		t.Fatal(err)
	}
	if err := f.s.settleAIRequest(f.ctx, id, "br_provider", aigateway.Outcome{Status: "failed", Dispatched: false}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	creditCounters(t, f, 100, 0, 0)
	id = newID("air_")
	if err := budgetReserve(t, f, id); err != nil {
		t.Fatal(err)
	}
	if err := f.s.settleAIRequest(f.ctx, id, "br_provider", aigateway.Outcome{Status: "usage_unknown", Dispatched: true}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	f.call("provider_operator", "PUT", creditPlatformPath, "disable-with-held-credit", record{"version": 2, "enabled": false}, 200)
	creditCounters(t, f, 100, 0, 26)
	f.call("provider_operator", "POST", "/api/v1/platform/ai-credits/requests/"+id+"/reconcile", "confirmed-nonacceptance-credit-release", record{"confirmed_not_accepted": true, "evidence_sha256": strings.Repeat("d", 64)}, 200)
	creditCounters(t, f, 100, 0, 0)
	// Disabled enforcement retains all prior history and ordinary free-mode
	// compatibility; it does not silently clear reserved or spent amounts.
	f.s.ai.Routes[0].Price = nil
	if err := budgetReserve(t, f, newID("air_")); err != nil {
		t.Fatal("disabled credit policy broke legacy admission", err)
	}
}

func TestAICreditsConcurrentBranchesAndActualOverrun(t *testing.T) {
	f := creditFixture(t, 26)
	f.exec(`INSERT INTO branches(id,project_id,name,state,created_at) VALUES('br_credit_sibling','prj_provider','credit-sibling','ready',now())`)
	var group sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 2)
	ids := make(chan string, 2)
	for _, branch := range []string{"br_provider", "br_credit_sibling"} {
		group.Add(1)
		go func(branch string) {
			defer group.Done()
			<-start
			id := newID("air_")
			ok, err := f.s.reserveAIRequest(f.ctx, id, "prj_provider", branch, "provider_operator", "", aigateway.ChatRequest{Model: "static-route", MaxTokens: 8})
			if err == nil && !ok {
				err = errors.New("unexpected rate denial")
			}
			if err == nil {
				ids <- id
			}
			results <- err
		}(branch)
	}
	close(start)
	group.Wait()
	close(results)
	passed, denied := 0, 0
	for err := range results {
		if err == nil {
			passed++
		} else if errors.Is(err, errAICreditsExceeded) {
			denied++
		} else {
			t.Fatal(err)
		}
	}
	if passed != 1 || denied != 1 {
		t.Fatal("cross-branch credit race", passed, denied)
	}
	creditCounters(t, f, 26, 0, 26)
	id := <-ids
	var branch string
	if err := f.s.db.QueryRow(f.ctx, `SELECT branch_id FROM ai_gateway_requests WHERE id=$1`, id).Scan(&branch); err != nil {
		t.Fatal(err)
	}
	if err := f.s.settleAIRequest(f.ctx, id, branch, aigateway.Outcome{Status: "completed", Dispatched: true, Usage: &aigateway.Usage{PromptTokens: 30, CompletionTokens: 5, TotalTokens: 35}}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	creditCounters(t, f, 26, 40, 0)
	if _, err := f.s.reserveAIRequest(context.Background(), newID("air_"), "prj_provider", "br_provider", "provider_operator", "", aigateway.ChatRequest{Model: "static-route", MaxTokens: 1}); !errors.Is(err, errAICreditsExceeded) {
		t.Fatal("overrun truth was capped or future call admitted", err)
	}
}

func TestAICreditsBudgetEndpointReconcilesBothLedgers(t *testing.T) {
	f := creditFixture(t, 100)
	f.call("provider_operator", "PUT", budgetPath, "dual-credit-budget-enable", record{"version": 0, "enabled": true, "limit_micros": 1000}, 200)
	id := newID("air_")
	if err := budgetReserve(t, f, id); err != nil {
		t.Fatal(err)
	}
	if err := f.s.settleAIRequest(f.ctx, id, "br_provider", aigateway.Outcome{Status: "incomplete", Dispatched: true}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	f.call("provider_operator", http.MethodPost, "/api/v1/projects/prj_provider/branches/br_provider/ai-gateway/requests/"+id+"/reconcile", "dual-existing-budget-reconcile", record{"usage": aigateway.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}, "confirmed_not_accepted": false, "evidence_sha256": strings.Repeat("e", 64)}, 200)
	creditCounters(t, f, 100, 7, 0)
	budgetCounters(t, f, 7, 0)
}

func TestAICreditsLimitedGatewayCannotIssueCredits(t *testing.T) {
	f := creditFixture(t, 100)
	var schema string
	if err := f.s.db.QueryRow(f.ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	role := newID("credits_ci_")
	f.exec("CREATE ROLE " + role + " LOGIN PASSWORD 'credits-ci-disposable'")
	f.exec("GRANT USAGE ON SCHEMA " + schema + " TO " + role)
	f.exec("GRANT SELECT ON ai_provider_catalog_state,ai_gateway_requests,ai_budget_policies,ai_budget_reservations,ai_credit_accounts,ai_credit_reservations TO " + role)
	f.exec("GRANT SELECT(id,project_id) ON branches TO " + role)
	f.exec("GRANT SELECT(id,org_id) ON projects TO " + role)
	f.exec("GRANT INSERT,UPDATE ON ai_gateway_requests TO " + role)
	f.exec("GRANT UPDATE(spent_micros,reserved_micros) ON ai_budget_policies,ai_credit_accounts TO " + role)
	f.exec("GRANT UPDATE(state,actual_micros,usage,settled_at) ON ai_budget_reservations,ai_credit_reservations TO " + role)
	f.exec("GRANT INSERT ON ai_credit_reservations TO " + role)
	f.exec("GRANT INSERT(id,org_id,request_id,kind,spent_delta,reserved_delta,actor_id,evidence_sha256) ON ai_credit_ledger TO " + role)
	cfg, err := pgxpool.ParseConfig(os.Getenv("NEON_V2_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User = role
	cfg.ConnConfig.Password = "credits-ci-disposable"
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
		t.Fatal("limited credit admission failed", err)
	}
	if err = limited.settleAIRequest(f.ctx, id, "br_provider", aigateway.Outcome{Status: "completed", Dispatched: true, Usage: &aigateway.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	creditCounters(t, f, 100, 7, 0)
	for _, sql := range []string{`UPDATE ai_credit_accounts SET credited_micros=1000000`, `UPDATE ai_credit_accounts SET enabled=false`, `UPDATE ai_credit_reservations SET price='{}'`, `DELETE FROM ai_credit_ledger`, `SELECT response FROM ai_credit_mutations`, `INSERT INTO ai_credit_ledger(id,org_id,kind,credit_delta,source_event_id,reason,actor_id,evidence_sha256) VALUES('forged','org_provider','grant',100,'forged-grant-event','forged credit grant','provider_operator',repeat('a',64))`} {
		if _, err = db.Exec(f.ctx, sql); err == nil {
			t.Fatalf("Gateway gained credit issuance or history authority: %s", sql)
		}
	}
}

func TestAICreditsHistoryCursorScopeAndUnsettledBound(t *testing.T) {
	f := creditFixture(t, 1)
	f.exec(`INSERT INTO ai_credit_ledger(id,org_id,kind,credit_delta,source_event_id,reason,actor_id,evidence_sha256,created_at) SELECT 'acl_'||lpad(to_hex(n),24,'0'),'org_provider','adjustment',1,'seed-credit-event-'||n,'Disposable pagination fixture','provider_operator',repeat('f',64),'2026-01-01T00:00:00Z'::timestamptz FROM generate_series(1,101) n`)
	f.exec(`UPDATE ai_credit_accounts SET credited_micros=credited_micros+101 WHERE org_id='org_provider'`)
	first := f.call("provider_org_admin", "GET", creditOrgPath+"/ledger", "", nil, 200)
	items := first["items"].([]any)
	if len(items) != 100 || stringVal(first["next_cursor"]) == "" {
		t.Fatal("history was silently truncated")
	}
	second := f.call("provider_org_admin", "GET", creditOrgPath+"/ledger?before="+stringVal(first["next_cursor"]), "", nil, 200)
	if len(second["items"].([]any)) != 2 || stringVal(second["next_cursor"]) != "" {
		t.Fatal("tied timestamp cursor skipped or duplicated entries")
	}
	seen := map[string]bool{}
	for _, item := range append(items, second["items"].([]any)...) {
		id := stringVal(item.(map[string]any)["id"])
		if seen[id] {
			t.Fatal("cursor repeated ledger row")
		}
		seen[id] = true
	}
	f.call("provider_org_admin", "GET", creditOrgPath+"/ledger?before=invalid_cursor", "", nil, 422)
	w := f.request("provider_org_admin", "GET", creditOrgPath, "", nil)
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("financial state can be cached")
	}
	// Expired transport occupancy never discards unresolved credit reservations.
	f.exec(`INSERT INTO ai_gateway_requests(id,project_id,branch_id,actor_id,model_id,route_generation,streaming,state,started_at,expires_at) SELECT 'air_'||lpad(to_hex(n),24,'0'),'prj_provider','br_provider','provider_operator','static-route',3,false,'usage_unknown',now()-interval '2 hours',now()-interval '1 hour' FROM generate_series(1,1024) n`)
	zero := &aigateway.Price{Version: "explicit-free", Currency: "USD", MaxInputTokens: 1}
	f.exec(`INSERT INTO ai_credit_reservations(request_id,org_id,project_id,branch_id,price,reserved_micros,state) SELECT id,'org_provider',project_id,branch_id,$1,0,'uncertain' FROM ai_gateway_requests`, zero)
	f.s.ai.Routes[0].Price = zero
	if err := budgetReserve(t, f, newID("air_")); !errors.Is(err, errAICreditsPending) {
		t.Fatal("unsettled credit ceiling ignored or expiry refunded it", err)
	}
	page := f.call("provider_org_admin", "GET", creditOrgPath+"/requests", "", nil, 200)
	if len(page["items"].([]any)) != 100 || stringVal(page["next_cursor"]) == "" {
		t.Fatal("unsettled request paging missing")
	}
	f.exec(`INSERT INTO organizations(id,slug,name) VALUES('org_credit_cursor_other','credit_cursor_other','Other')`)
	other := f.call("provider_operator", "GET", "/api/v1/platform/ai-credits/organizations/org_credit_cursor_other/ledger?before="+stringVal(first["next_cursor"]), "", nil, 200)
	if len(other["items"].([]any)) != 0 {
		t.Fatal("cursor escaped organization scope")
	}
}

func TestAICreditsActualClientCancellationKeepsReservation(t *testing.T) {
	f := creditFixture(t, 26)
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
	body := []byte(`{"model":"static-route","input":"credit-cancel-after-acceptance","max_output_tokens":8,"stream":true}`)
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
		t.Fatal("supplier did not accept request")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("independent credit settlement did not finish")
	}
	creditCounters(t, f, 26, 0, 26)
	var state string
	var total *int64
	if err := f.s.db.QueryRow(f.ctx, `SELECT c.state,a.total_tokens FROM ai_credit_reservations c JOIN ai_gateway_requests a ON a.id=c.request_id WHERE c.request_id=$1`, w.Header().Get("X-Request-ID")).Scan(&state, &total); err != nil || state != "uncertain" || total != nil {
		t.Fatal("client cancellation made accepted unknown credit free", err)
	}
}
