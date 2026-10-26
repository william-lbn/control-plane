package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type record = map[string]any

func (s *server) one(ctx context.Context, query string, args ...any) (record, error) {
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgx.CollectOneRow(rows, pgx.RowToMap)
}
func (s *server) many(ctx context.Context, query string, args ...any) ([]record, error) {
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []record{}
	}
	return items, nil
}

func (s *server) routes() *http.ServeMux {
	// Provider-authenticated durable inbox; browser sessions cannot fund invoices.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhooks/billing/stripe", s.billingWebhook)
	mux.Handle("GET /api/v1/platform/ai-providers/capabilities", s.auth(http.HandlerFunc(s.aiProviderCapabilities), false))
	mux.Handle("GET /api/v1/platform/ai-providers", s.auth(http.HandlerFunc(s.aiProviderRead), false))
	mux.Handle("POST /api/v1/platform/ai-providers", s.auth(http.HandlerFunc(s.aiProviderDraft), true))
	mux.Handle("GET /api/v1/platform/ai-providers/{provider}", s.auth(http.HandlerFunc(s.aiProviderRead), false))
	mux.Handle("POST /api/v1/platform/ai-providers/{provider}/revisions", s.auth(http.HandlerFunc(s.aiProviderDraft), true))
	mux.Handle("POST /api/v1/platform/ai-providers/{provider}/publish", s.auth(http.HandlerFunc(s.aiProviderPublish), true))
	mux.Handle("POST /api/v1/platform/ai-providers/{provider}/disable", s.auth(http.HandlerFunc(s.aiProviderDisable), true))
	mux.Handle("DELETE /api/v1/platform/ai-providers/{provider}", s.auth(http.HandlerFunc(s.aiProviderDisable), true))
	mux.Handle("GET /api/v1/platform/ai-provider-operations/{providerOperation}", s.auth(http.HandlerFunc(s.aiProviderOperation), false))
	mux.Handle("POST /api/v1/platform/ai-provider-operations/{providerOperation}/retry", s.auth(http.HandlerFunc(s.aiProviderRetry), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/functions-copy", s.auth(http.HandlerFunc(s.copyFunctionBranch), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/function-copy-plans", s.auth(http.HandlerFunc(s.functionBranchCopyPlans), false))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/functions/{slug}/output-policy", s.auth(http.HandlerFunc(s.functionOutputPolicy), false))
	mux.Handle("PATCH /api/v1/projects/{project}/branches/{branch}/functions/{slug}/output-policy", s.auth(http.HandlerFunc(s.functionOutputPolicy), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/functions/{slug}/output-logs", s.auth(http.HandlerFunc(s.functionOutputLogs), false))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/functions-triggers", s.auth(http.HandlerFunc(s.functionTriggers), false))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/functions-triggers", s.auth(http.HandlerFunc(s.functionTriggers), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/functions-triggers/{trigger}", s.auth(http.HandlerFunc(s.functionTriggers), false))
	mux.Handle("PATCH /api/v1/projects/{project}/branches/{branch}/functions-triggers/{trigger}", s.auth(http.HandlerFunc(s.functionTriggers), true))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}/functions-triggers/{trigger}", s.auth(http.HandlerFunc(s.functionTriggers), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/functions-triggers/{trigger}/invocations", s.auth(http.HandlerFunc(s.functionTriggerInvocations), false))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/functions-triggers/{trigger}/invocations/{invocation}/retry", s.auth(http.HandlerFunc(s.retryFunctionObjectEvent), true))
	mux.Handle("PATCH /api/v1/projects/{project}/branches/{branch}/functions/{slug}/http-policy", s.auth(http.HandlerFunc(s.functionHTTPPolicy), true))
	mux.Handle("GET /api/v1/projects/{project}/saved-queries", s.auth(http.HandlerFunc(s.savedQueries), false))
	mux.Handle("POST /api/v1/projects/{project}/saved-queries", s.auth(http.HandlerFunc(s.savedQueries), true))
	mux.Handle("GET /api/v1/projects/{project}/saved-queries/{query}", s.auth(http.HandlerFunc(s.savedQueries), false))
	mux.Handle("PATCH /api/v1/projects/{project}/saved-queries/{query}", s.auth(http.HandlerFunc(s.savedQueries), true))
	mux.Handle("DELETE /api/v1/projects/{project}/saved-queries/{query}", s.auth(http.HandlerFunc(s.savedQueries), true))
	mux.Handle("POST /api/v1/projects/{project}/saved-queries/{query}/sharing", s.auth(http.HandlerFunc(s.savedQueries), true))
	mux.Handle("GET /api/v1/projects/{project}/query-history", s.auth(http.HandlerFunc(s.sqlHistory), false))
	mux.Handle("GET /api/v1/projects/{project}/query-history/{history}", s.auth(http.HandlerFunc(s.sqlHistory), false))
	mux.Handle("DELETE /api/v1/projects/{project}/query-history/{history}", s.auth(http.HandlerFunc(s.deleteSQLHistory), true))
	mux.Handle("GET /api/v1/projects/{project}/query-history/{history}/sharing", s.auth(http.HandlerFunc(s.sqlHistorySharing), false))
	mux.Handle("GET /api/v1/projects/{project}/query-history/{history}/name", s.auth(http.HandlerFunc(s.sqlHistoryName), false))
	mux.Handle("PUT /api/v1/projects/{project}/query-history/{history}/name", s.auth(http.HandlerFunc(s.sqlHistoryName), true))
	mux.Handle("PUT /api/v1/projects/{project}/query-history/{history}/sharing", s.auth(http.HandlerFunc(s.sqlHistorySharing), true))
	mux.Handle("GET /api/v1/projects/{project}/shared-query-history", s.auth(http.HandlerFunc(s.sharedSQLHistory), false))
	mux.Handle("GET /api/v1/projects/{project}/shared-query-history/{history}", s.auth(http.HandlerFunc(s.sharedSQLHistory), false))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/ai-gateway", s.auth(http.HandlerFunc(s.aiRead), false))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/ai-gateway/usage", s.auth(http.HandlerFunc(s.aiRead), false))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/ai-gateway/budget", s.auth(http.HandlerFunc(s.aiBudgetRead), false))
	mux.Handle("PUT /api/v1/projects/{project}/branches/{branch}/ai-gateway/budget", s.auth(http.HandlerFunc(s.aiBudgetPut), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/ai-gateway/requests/{aiRequest}/reconcile", s.auth(http.HandlerFunc(s.aiBudgetReconcile), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/ai-gateway/request", s.auth(http.HandlerFunc(s.aiConsoleRequest), true))
	mux.HandleFunc("GET /ai/v1/{aiBranch}/v1/models", s.aiRelay)
	mux.HandleFunc("GET /ai/v1/{aiBranch}/v1/responses/{providerResponse}", s.aiProviderStateRelay)
	mux.HandleFunc("DELETE /ai/v1/{aiBranch}/v1/responses/{providerResponse}", s.aiProviderStateRelay)
	mux.HandleFunc("GET /ai/v1/{aiBranch}/v1/responses/{providerResponse}/input_items", s.aiProviderStateRelay)
	mux.HandleFunc("POST /ai/v1/{aiBranch}/v1/conversations", s.aiProviderStateRelay)
	mux.HandleFunc("GET /ai/v1/{aiBranch}/v1/conversations/{providerConversation}", s.aiProviderStateRelay)
	mux.HandleFunc("POST /ai/v1/{aiBranch}/v1/conversations/{providerConversation}", s.aiProviderStateRelay)
	mux.HandleFunc("DELETE /ai/v1/{aiBranch}/v1/conversations/{providerConversation}", s.aiProviderStateRelay)
	mux.HandleFunc("POST /ai/v1/{aiBranch}/v1/conversations/{providerConversation}/items", s.aiProviderStateRelay)
	mux.HandleFunc("GET /ai/v1/{aiBranch}/v1/conversations/{providerConversation}/items", s.aiProviderStateRelay)
	mux.HandleFunc("GET /ai/v1/{aiBranch}/v1/conversations/{providerConversation}/items/{providerItem}", s.aiProviderStateRelay)
	mux.HandleFunc("DELETE /ai/v1/{aiBranch}/v1/conversations/{providerConversation}/items/{providerItem}", s.aiProviderStateRelay)
	mux.HandleFunc("GET /ai/v1/{aiBranch}/openai/v1/responses/{providerResponse}", s.aiProviderStateRelay)
	mux.HandleFunc("DELETE /ai/v1/{aiBranch}/openai/v1/responses/{providerResponse}", s.aiProviderStateRelay)
	mux.HandleFunc("GET /ai/v1/{aiBranch}/openai/v1/responses/{providerResponse}/input_items", s.aiProviderStateRelay)
	mux.HandleFunc("POST /ai/v1/{aiBranch}/openai/v1/conversations", s.aiProviderStateRelay)
	mux.HandleFunc("GET /ai/v1/{aiBranch}/openai/v1/conversations/{providerConversation}", s.aiProviderStateRelay)
	mux.HandleFunc("POST /ai/v1/{aiBranch}/openai/v1/conversations/{providerConversation}", s.aiProviderStateRelay)
	mux.HandleFunc("DELETE /ai/v1/{aiBranch}/openai/v1/conversations/{providerConversation}", s.aiProviderStateRelay)
	mux.HandleFunc("POST /ai/v1/{aiBranch}/openai/v1/conversations/{providerConversation}/items", s.aiProviderStateRelay)
	mux.HandleFunc("GET /ai/v1/{aiBranch}/openai/v1/conversations/{providerConversation}/items", s.aiProviderStateRelay)
	mux.HandleFunc("GET /ai/v1/{aiBranch}/openai/v1/conversations/{providerConversation}/items/{providerItem}", s.aiProviderStateRelay)
	mux.HandleFunc("DELETE /ai/v1/{aiBranch}/openai/v1/conversations/{providerConversation}/items/{providerItem}", s.aiProviderStateRelay)
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/v1/responses/{providerResponse}", s.auth(http.HandlerFunc(s.aiConsoleProviderState), false))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/v1/responses/{providerResponse}", s.auth(http.HandlerFunc(s.aiConsoleProviderState), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/v1/responses/{providerResponse}/input_items", s.auth(http.HandlerFunc(s.aiConsoleProviderState), false))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/v1/responses/{providerResponse}/reconcile", s.auth(http.HandlerFunc(s.aiConsoleProviderState), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/v1/conversations", s.auth(http.HandlerFunc(s.aiConsoleProviderState), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/v1/conversations/{providerConversation}", s.auth(http.HandlerFunc(s.aiConsoleProviderState), false))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/v1/conversations/{providerConversation}", s.auth(http.HandlerFunc(s.aiConsoleProviderState), true))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/v1/conversations/{providerConversation}", s.auth(http.HandlerFunc(s.aiConsoleProviderState), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/v1/conversations/{providerConversation}/reconcile", s.auth(http.HandlerFunc(s.aiConsoleProviderState), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/v1/conversations/{providerConversation}/items", s.auth(http.HandlerFunc(s.aiConsoleProviderState), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/v1/conversations/{providerConversation}/items", s.auth(http.HandlerFunc(s.aiConsoleProviderState), false))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/v1/conversations/{providerConversation}/items/{providerItem}", s.auth(http.HandlerFunc(s.aiConsoleProviderState), false))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/v1/conversations/{providerConversation}/items/{providerItem}", s.auth(http.HandlerFunc(s.aiConsoleProviderState), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/openai/v1/responses/{providerResponse}", s.auth(http.HandlerFunc(s.aiConsoleProviderState), false))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/openai/v1/responses/{providerResponse}", s.auth(http.HandlerFunc(s.aiConsoleProviderState), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/openai/v1/responses/{providerResponse}/input_items", s.auth(http.HandlerFunc(s.aiConsoleProviderState), false))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/openai/v1/responses/{providerResponse}/reconcile", s.auth(http.HandlerFunc(s.aiConsoleProviderState), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/openai/v1/conversations", s.auth(http.HandlerFunc(s.aiConsoleProviderState), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/openai/v1/conversations/{providerConversation}", s.auth(http.HandlerFunc(s.aiConsoleProviderState), false))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/openai/v1/conversations/{providerConversation}", s.auth(http.HandlerFunc(s.aiConsoleProviderState), true))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/openai/v1/conversations/{providerConversation}", s.auth(http.HandlerFunc(s.aiConsoleProviderState), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/openai/v1/conversations/{providerConversation}/reconcile", s.auth(http.HandlerFunc(s.aiConsoleProviderState), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/openai/v1/conversations/{providerConversation}/items", s.auth(http.HandlerFunc(s.aiConsoleProviderState), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/openai/v1/conversations/{providerConversation}/items", s.auth(http.HandlerFunc(s.aiConsoleProviderState), false))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/openai/v1/conversations/{providerConversation}/items/{providerItem}", s.auth(http.HandlerFunc(s.aiConsoleProviderState), false))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}/ai-gateway/state/openai/v1/conversations/{providerConversation}/items/{providerItem}", s.auth(http.HandlerFunc(s.aiConsoleProviderState), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/ai-gateway/provider-state", s.auth(http.HandlerFunc(s.aiProviderStateList), false))
	mux.HandleFunc("GET /ai/v1/{aiBranch}/v1/files", s.aiRelay)
	mux.HandleFunc("POST /ai/v1/{aiBranch}/v1/files", s.aiRelay)
	mux.HandleFunc("GET /ai/v1/{aiBranch}/v1/files/{inputFile}", s.aiRelay)
	mux.HandleFunc("GET /ai/v1/{aiBranch}/v1/files/{inputFile}/content", s.aiRelay)
	mux.HandleFunc("DELETE /ai/v1/{aiBranch}/v1/files/{inputFile}", s.aiRelay)

	mux.HandleFunc("POST /ai/v1/{aiBranch}/v1/chat/completions", s.aiRelay)
	mux.HandleFunc("POST /ai/v1/{aiBranch}/v1/responses", s.aiRelay)
	mux.HandleFunc("POST /ai/v1/{aiBranch}/openai/v1/responses", s.aiRelay)
	mux.HandleFunc("POST /ai/v1/{aiBranch}/v1/embeddings", s.aiRelay)
	mux.HandleFunc("POST /ai/v1/{aiBranch}/openai/v1/embeddings", s.aiRelay)
	mux.HandleFunc("POST /ai/v1/{aiBranch}/v1/messages", s.aiRelay)
	mux.HandleFunc("POST /ai/v1/{aiBranch}/anthropic/v1/messages", s.aiRelay)
	mux.HandleFunc("POST /ai/v1/{aiBranch}/gemini/v1beta/models/{aiModelAction}", s.aiRelay)
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/ai-gateway/request/{protocol}", s.auth(http.HandlerFunc(s.aiConsoleRequest), true))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := s.db.Ping(ctx); err != nil {
			fail(w, r, 503, "metadata_unavailable", "Control database unavailable")
			return
		}
		jsonResponse(w, 200, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("POST /auth/login", s.login)
	mux.HandleFunc("GET /auth/sso/provider", s.consoleSSOPublic)
	mux.HandleFunc("POST /auth/sso/start", s.consoleSSOStart)
	mux.HandleFunc("GET /auth/sso/callback", s.consoleSSOCallback)
	mux.HandleFunc("POST /auth/sso/mfa", s.consoleSSOMFA)
	mux.Handle("GET /api/v1/organizations/{org}/sso", s.auth(http.HandlerFunc(s.consoleSSOSettings), false))
	mux.Handle("PUT /api/v1/organizations/{org}/sso", s.auth(http.HandlerFunc(s.consoleSSOSettings), true))
	mux.Handle("POST /api/v1/organizations/{org}/sso/link", s.auth(http.HandlerFunc(s.consoleSSOStart), true))
	mux.Handle("DELETE /api/v1/organizations/{org}/sso/bindings/{binding}", s.auth(http.HandlerFunc(s.consoleSSOUnlink), true))
	mux.HandleFunc("POST /auth/signup", s.signup)
	mux.Handle("POST /api/v1/invitations/accept", s.auth(http.HandlerFunc(s.acceptInvitation), true))
	mux.Handle("POST /auth/logout", s.auth(http.HandlerFunc(s.logout), true))
	mux.Handle("GET /api/v1/session", s.auth(http.HandlerFunc(s.session), false))
	mux.Handle("GET /api/v1/account/sessions", s.auth(http.HandlerFunc(s.consoleSessions), false))
	mux.Handle("GET /api/v1/account/mfa", s.auth(http.HandlerFunc(s.consoleMFA), false))
	mux.Handle("GET /api/v1/account/query-history-policy", s.auth(http.HandlerFunc(s.sqlHistoryPolicy), false))
	mux.Handle("PATCH /api/v1/account/query-history-policy", s.auth(http.HandlerFunc(s.sqlHistoryPolicy), true))
	mux.Handle("POST /api/v1/account/query-history-cleanup", s.auth(http.HandlerFunc(s.sqlHistoryCleanup), true))
	mux.Handle("POST /api/v1/account/mfa/enroll", s.auth(http.HandlerFunc(s.consoleMFA), true))
	mux.Handle("POST /api/v1/account/mfa/confirm", s.auth(http.HandlerFunc(s.consoleMFA), true))
	mux.Handle("POST /api/v1/account/mfa/disable", s.auth(http.HandlerFunc(s.consoleMFA), true))
	mux.Handle("POST /api/v1/account/sessions/{session}/revoke", s.auth(http.HandlerFunc(s.revokeConsoleSessions), true))
	mux.Handle("POST /api/v1/account/sessions/revoke-others", s.auth(http.HandlerFunc(s.revokeConsoleSessions), true))
	mux.Handle("GET /api/v1/capabilities", s.auth(http.HandlerFunc(s.capabilities), false))
	mux.Handle("GET /api/v1/organizations", s.auth(http.HandlerFunc(s.organizations), false))
	mux.Handle("POST /api/v1/organizations", s.auth(http.HandlerFunc(s.organizations), true))
	mux.Handle("GET /api/v1/organizations/{org}", s.auth(http.HandlerFunc(s.organizationDetail), false))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/ai-gateway/files", s.auth(http.HandlerFunc(s.aiInputFiles), false))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/ai-gateway/files", s.auth(http.HandlerFunc(s.aiInputFiles), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/ai-gateway/files/{inputFile}", s.auth(http.HandlerFunc(s.aiInputFiles), false))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/ai-gateway/files/{inputFile}/content", s.auth(http.HandlerFunc(s.aiInputFiles), false))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}/ai-gateway/files/{inputFile}", s.auth(http.HandlerFunc(s.aiInputFiles), true))
	mux.Handle("GET /api/v1/organizations/{org}/ai-providers/capabilities", s.auth(http.HandlerFunc(s.aiProviderOrganizationCapabilities), false))
	mux.Handle("GET /api/v1/organizations/{org}/ai-providers", s.auth(http.HandlerFunc(s.aiProviderRead), false))
	mux.Handle("POST /api/v1/organizations/{org}/ai-providers", s.auth(http.HandlerFunc(s.aiProviderDraft), true))
	mux.Handle("GET /api/v1/organizations/{org}/ai-providers/{provider}", s.auth(http.HandlerFunc(s.aiProviderRead), false))
	mux.Handle("POST /api/v1/organizations/{org}/ai-providers/{provider}/revisions", s.auth(http.HandlerFunc(s.aiProviderDraft), true))
	mux.Handle("POST /api/v1/organizations/{org}/ai-providers/{provider}/publish", s.auth(http.HandlerFunc(s.aiProviderPublish), true))
	mux.Handle("POST /api/v1/organizations/{org}/ai-providers/{provider}/disable", s.auth(http.HandlerFunc(s.aiProviderDisable), true))
	mux.Handle("DELETE /api/v1/organizations/{org}/ai-providers/{provider}", s.auth(http.HandlerFunc(s.aiProviderDisable), true))
	mux.Handle("GET /api/v1/organizations/{org}/ai-provider-operations/{providerOperation}", s.auth(http.HandlerFunc(s.aiProviderOperation), false))
	mux.Handle("POST /api/v1/organizations/{org}/ai-provider-operations/{providerOperation}/retry", s.auth(http.HandlerFunc(s.aiProviderRetry), true))
	mux.Handle("GET /api/v1/organizations/{org}/ai-credits", s.auth(http.HandlerFunc(s.aiCreditRead), false))
	mux.Handle("GET /api/v1/organizations/{org}/billing", s.auth(http.HandlerFunc(s.billingCatalog), false))
	mux.Handle("POST /api/v1/organizations/{org}/billing/invoices", s.auth(http.HandlerFunc(s.billingCreateInvoice), true))
	mux.Handle("GET /api/v1/organizations/{org}/billing/invoices", s.auth(http.HandlerFunc(s.billingReadInvoices), false))
	mux.Handle("GET /api/v1/organizations/{org}/billing/invoices/{invoice}", s.auth(http.HandlerFunc(s.billingReadInvoices), false))
	mux.Handle("POST /api/v1/organizations/{org}/billing/invoices/{invoice}/checkout", s.auth(http.HandlerFunc(s.billingQueueCheckout), true))
	mux.Handle("GET /api/v1/organizations/{org}/billing/invoices/{invoice}/checkout", s.auth(http.HandlerFunc(s.billingCheckoutLink), false))
	mux.Handle("GET /api/v1/platform/billing/organizations/{billingOrg}/invoices", s.auth(http.HandlerFunc(s.billingReadInvoices), false))
	mux.Handle("GET /api/v1/platform/billing/organizations/{billingOrg}/invoices/{invoice}", s.auth(http.HandlerFunc(s.billingReadInvoices), false))
	mux.Handle("POST /api/v1/platform/billing/organizations/{billingOrg}/invoices/{invoice}/reconcile", s.auth(http.HandlerFunc(s.billingReconcile), true))
	mux.Handle("GET /api/v1/platform/billing/organizations/{billingOrg}/invoices/{invoice}/receipts", s.auth(http.HandlerFunc(s.billingReadReceipts), false))
	mux.Handle("GET /api/v1/organizations/{org}/billing/invoices/{invoice}/receipts", s.auth(http.HandlerFunc(s.billingReadReceipts), false))
	mux.Handle("GET /api/v1/service-plans", s.auth(http.HandlerFunc(s.servicePlanCatalog), false))
	mux.Handle("GET /api/v1/service-plans/{servicePlan}", s.auth(http.HandlerFunc(s.servicePlanCatalog), false))
	mux.Handle("GET /api/v1/platform/service-plans", s.auth(http.HandlerFunc(s.servicePlanCatalog), false))
	mux.Handle("POST /api/v1/platform/service-plans", s.auth(http.HandlerFunc(s.servicePlanPublish), true))
	mux.Handle("GET /api/v1/platform/service-plans/{servicePlan}", s.auth(http.HandlerFunc(s.servicePlanCatalog), false))
	mux.Handle("POST /api/v1/platform/service-plans/{servicePlan}/revisions", s.auth(http.HandlerFunc(s.servicePlanPublish), true))
	mux.Handle("PUT /api/v1/platform/service-plans/{servicePlan}/state", s.auth(http.HandlerFunc(s.servicePlanState), true))
	mux.Handle("GET /api/v1/platform/organizations/{planOrg}/service-plan", s.auth(http.HandlerFunc(s.organizationServicePlan), false))
	mux.Handle("PUT /api/v1/platform/organizations/{planOrg}/service-plan", s.auth(http.HandlerFunc(s.organizationServicePlan), true))
	mux.Handle("GET /api/v1/organizations/{org}/service-plan", s.auth(http.HandlerFunc(s.organizationServicePlan), false))
	mux.Handle("GET /api/v1/organizations/{org}/ai-credits/ledger", s.auth(http.HandlerFunc(s.aiCreditRead), false))
	mux.Handle("GET /api/v1/organizations/{org}/ai-credits/requests", s.auth(http.HandlerFunc(s.aiCreditRead), false))
	mux.Handle("GET /api/v1/platform/ai-credits/organizations/{creditOrg}", s.auth(http.HandlerFunc(s.aiCreditRead), false))
	mux.Handle("GET /api/v1/platform/ai-credits/organizations/{creditOrg}/ledger", s.auth(http.HandlerFunc(s.aiCreditRead), false))
	mux.Handle("GET /api/v1/platform/ai-credits/organizations/{creditOrg}/requests", s.auth(http.HandlerFunc(s.aiCreditRead), false))
	mux.Handle("PUT /api/v1/platform/ai-credits/organizations/{creditOrg}", s.auth(http.HandlerFunc(s.aiCreditPolicy), true))
	mux.Handle("POST /api/v1/platform/ai-credits/organizations/{creditOrg}/entries", s.auth(http.HandlerFunc(s.aiCreditEntry), true))
	mux.Handle("POST /api/v1/platform/ai-credits/requests/{creditRequest}/reconcile", s.auth(http.HandlerFunc(s.aiCreditReconcile), true))
	mux.Handle("GET /api/v1/organizations/{org}/members", s.auth(http.HandlerFunc(s.organizationMembers), false))
	mux.Handle("GET /api/v1/organizations/{org}/invitations", s.auth(http.HandlerFunc(s.consoleInvitations), false))
	mux.Handle("POST /api/v1/organizations/{org}/invitations", s.auth(http.HandlerFunc(s.consoleInvitations), true))
	mux.Handle("DELETE /api/v1/organizations/{org}/invitations/{invitation}", s.auth(http.HandlerFunc(s.revokeConsoleInvitation), true))
	mux.Handle("POST /api/v1/organizations/{org}/members", s.auth(http.HandlerFunc(s.organizationMembers), true))
	mux.Handle("PATCH /api/v1/organizations/{org}/members/{member}", s.auth(http.HandlerFunc(s.changeMember), true))
	mux.Handle("DELETE /api/v1/organizations/{org}/members/{member}", s.auth(http.HandlerFunc(s.changeMember), true))
	mux.Handle("GET /api/v1/projects/{project}/permissions", s.auth(http.HandlerFunc(s.projectPermissions), false))
	mux.Handle("PUT /api/v1/projects/{project}/permissions/{member}", s.auth(http.HandlerFunc(s.projectPermissions), true))
	mux.Handle("DELETE /api/v1/projects/{project}/permissions/{member}", s.auth(http.HandlerFunc(s.projectPermissions), true))
	mux.Handle("GET /api/v1/api-keys", s.auth(http.HandlerFunc(s.apiKeys), false))
	mux.Handle("POST /api/v1/api-keys", s.auth(http.HandlerFunc(s.apiKeys), true))
	mux.Handle("DELETE /api/v1/api-keys/{key}", s.auth(http.HandlerFunc(s.apiKeys), true))
	mux.Handle("GET /api/v1/organizations/{org}/api-keys", s.auth(http.HandlerFunc(s.apiKeys), false))
	mux.Handle("POST /api/v1/organizations/{org}/api-keys", s.auth(http.HandlerFunc(s.apiKeys), true))
	mux.Handle("DELETE /api/v1/organizations/{org}/api-keys/{key}", s.auth(http.HandlerFunc(s.apiKeys), true))
	mux.Handle("GET /api/v1/organizations/{org}/projects", s.auth(http.HandlerFunc(s.projects), false))
	mux.Handle("POST /api/v1/organizations/{org}/projects", s.auth(http.HandlerFunc(s.createProject), true))
	mux.Handle("GET /api/v1/projects/{project}", s.auth(http.HandlerFunc(s.project), false))
	mux.Handle("DELETE /api/v1/projects/{project}", s.auth(http.HandlerFunc(s.deleteResource), true))
	mux.Handle("GET /api/v1/projects/{project}/lifecycle", s.auth(http.HandlerFunc(s.lifecycleOverview), false))
	mux.Handle("PATCH /api/v1/projects/{project}/protection", s.auth(http.HandlerFunc(s.setResourceProtection), true))
	mux.Handle("POST /api/v1/projects/{project}/recover", s.auth(http.HandlerFunc(s.recoverProject), true))
	mux.Handle("GET /api/v1/projects/{project}/branches", s.auth(http.HandlerFunc(s.branches), false))
	mux.Handle("POST /api/v1/projects/{project}/branches", s.auth(http.HandlerFunc(s.createBranch), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}", s.auth(http.HandlerFunc(s.branch), false))
	mux.Handle("PATCH /api/v1/projects/{project}/branches/{branch}/expiration", s.auth(http.HandlerFunc(s.setBranchExpiration), true))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}", s.auth(http.HandlerFunc(s.deleteResource), true))
	mux.Handle("PATCH /api/v1/projects/{project}/branches/{branch}/protection", s.auth(http.HandlerFunc(s.setResourceProtection), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/restore-window", s.auth(http.HandlerFunc(s.readRestoreWindow), false))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/restore-point", s.auth(http.HandlerFunc(s.previewRestorePoint), false))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/services", s.auth(http.HandlerFunc(s.branchServices), false))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/storage", s.auth(http.HandlerFunc(s.objectStorageRead), false))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/storage", s.auth(http.HandlerFunc(s.objectStorageMutate), true))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}/storage", s.auth(http.HandlerFunc(s.objectStorageMutate), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/objects/settings", s.auth(http.HandlerFunc(s.storageObjectSettings), false))
	mux.Handle("PUT /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/objects/settings", s.auth(http.HandlerFunc(s.storageObjectSettings), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/settings", s.auth(http.HandlerFunc(s.storageBucketSettings), false))
	mux.Handle("PUT /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/settings", s.auth(http.HandlerFunc(s.storageBucketSettings), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/storage/buckets", s.auth(http.HandlerFunc(s.storageBuckets), false))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/storage/buckets", s.auth(http.HandlerFunc(s.storageBuckets), true))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}", s.auth(http.HandlerFunc(s.storageBucketDelete), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/objects", s.auth(http.HandlerFunc(s.storageObjects), false))
	mux.Handle("HEAD /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/objects", s.auth(http.HandlerFunc(s.storageObjects), false))
	mux.Handle("PUT /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/objects", s.auth(http.HandlerFunc(s.storageObjects), true))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/objects", s.auth(http.HandlerFunc(s.storageObjects), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/presign", s.auth(http.HandlerFunc(s.storagePresign), true))
	mux.Handle("PATCH /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/objects/metadata", s.auth(http.HandlerFunc(s.storageMetadataPatch), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/copy", s.auth(http.HandlerFunc(s.storageCopy), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/delete-batch", s.auth(http.HandlerFunc(s.storageDeleteBatch), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/multipart-uploads", s.auth(http.HandlerFunc(s.storageMultipart), false))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/multipart-uploads", s.auth(http.HandlerFunc(s.storageMultipart), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/multipart-uploads/{upload}", s.auth(http.HandlerFunc(s.storageMultipart), false))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/multipart-uploads/{upload}", s.auth(http.HandlerFunc(s.storageMultipart), true))
	mux.Handle("PUT /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/multipart-uploads/{upload}/parts/{part}", s.auth(http.HandlerFunc(s.storageMultipart), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/multipart-uploads/{upload}/complete", s.auth(http.HandlerFunc(s.storageMultipart), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/multipart-administration", s.auth(http.HandlerFunc(s.storageMultipartAdmin), false))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/multipart-administration/{upload}/abort", s.auth(http.HandlerFunc(s.storageMultipartAdmin), true))
	mux.HandleFunc("GET /storage/v1/{storageBranch}/{bucket}", s.storagePublic)
	mux.HandleFunc("HEAD /storage/v1/{storageBranch}/{bucket}", s.storagePublic)
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/presign-upload", s.auth(http.HandlerFunc(s.storagePresignUpload), true))
	mux.HandleFunc("PUT /storage/v1/{storageBranch}/{bucket}", s.storagePublicUpload)
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/cors", s.auth(http.HandlerFunc(s.storageBucketCORS), false))
	mux.Handle("PUT /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/cors", s.auth(http.HandlerFunc(s.storageBucketCORS), true))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}/storage/buckets/{bucket}/cors", s.auth(http.HandlerFunc(s.storageBucketCORS), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/data-api", s.auth(http.HandlerFunc(s.dataAPIRead), false))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/auth", s.auth(http.HandlerFunc(s.managedAuthRead), false))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/functions", s.auth(http.HandlerFunc(s.functionsList), false))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/functions", s.auth(http.HandlerFunc(s.functionCreate), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/functions/{slug}/deployments", s.auth(http.HandlerFunc(s.functionDeploy), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/functions/{slug}/console-invoke", s.auth(http.HandlerFunc(s.functionConsoleInvoke), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/functions/{slug}/suspend", s.auth(http.HandlerFunc(s.functionStop), true))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}/functions/{slug}", s.auth(http.HandlerFunc(s.functionStop), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/functions/{slug}", s.auth(http.HandlerFunc(s.functionRead), false))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/functions/{slug}/runtime", s.auth(http.HandlerFunc(s.functionRuntimeRead), false))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/functions/{slug}/resume", s.auth(http.HandlerFunc(s.functionResume), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/functions/{slug}/rollback", s.auth(http.HandlerFunc(s.functionRollback), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/functions/{slug}/deployments", s.auth(http.HandlerFunc(s.functionDeployments), false))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/functions/{slug}/request-logs", s.auth(http.HandlerFunc(s.functionRequestLogs), false))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/auth", s.auth(http.HandlerFunc(s.managedAuthMutate), true))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}/auth", s.auth(http.HandlerFunc(s.managedAuthMutate), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/auth/users", s.auth(http.HandlerFunc(s.managedAuthUsers), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/data-api", s.auth(http.HandlerFunc(s.dataAPIMutate), true))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}/data-api", s.auth(http.HandlerFunc(s.dataAPIMutate), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/data-api/request", s.auth(http.HandlerFunc(s.dataAPIConsoleRequest), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/credentials", s.auth(http.HandlerFunc(s.backendCredentials), false))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/credentials", s.auth(http.HandlerFunc(s.backendCredentials), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/credentials/check", s.auth(http.HandlerFunc(s.checkBackendCredential), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/credentials/{credential}/rotate", s.auth(http.HandlerFunc(s.mutateBackendCredential), true))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}/credentials/{credential}", s.auth(http.HandlerFunc(s.mutateBackendCredential), true))
	// Application data requests authenticate at the branch gateway, independently
	// of Console cookies. The handler validates methods and branch ownership.
	mux.HandleFunc("/data/v1/{dataBranch}/{rest...}", s.dataAPIRelay)
	mux.HandleFunc("/auth/v1/{authBranch}/{rest...}", s.managedAuthRelay)
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/roles", s.auth(http.HandlerFunc(s.catalogList), false))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/roles", s.auth(http.HandlerFunc(s.catalogMutation), true))
	mux.Handle("PATCH /api/v1/projects/{project}/branches/{branch}/roles/{role}", s.auth(http.HandlerFunc(s.catalogMutation), true))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}/roles/{role}", s.auth(http.HandlerFunc(s.catalogMutation), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/roles/{role}/rename", s.auth(http.HandlerFunc(s.catalogRenameMutation), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/databases", s.auth(http.HandlerFunc(s.catalogList), false))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/databases", s.auth(http.HandlerFunc(s.catalogMutation), true))
	mux.Handle("PATCH /api/v1/projects/{project}/branches/{branch}/databases/{database}", s.auth(http.HandlerFunc(s.databaseOwnerMutation), true))
	mux.Handle("DELETE /api/v1/projects/{project}/branches/{branch}/databases/{database}", s.auth(http.HandlerFunc(s.catalogMutation), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/databases/{database}/rename", s.auth(http.HandlerFunc(s.catalogRenameMutation), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/connection-defaults", s.auth(http.HandlerFunc(s.connectionDefaults), false))
	mux.Handle("PATCH /api/v1/projects/{project}/branches/{branch}/connection-defaults", s.auth(http.HandlerFunc(s.connectionDefaults), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/catalog-identities", s.auth(http.HandlerFunc(s.catalogIdentities), false))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/catalog-platform-identities", s.auth(http.HandlerFunc(s.catalogPlatformIdentities), false))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/catalog-platform-identities", s.auth(http.HandlerFunc(s.catalogPlatformIdentities), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/catalog-identities", s.auth(http.HandlerFunc(s.catalogIdentities), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/catalog-identities/{binding}/restore", s.auth(http.HandlerFunc(s.catalogIdentityRestore), true))
	mux.Handle("POST /api/v1/projects/{project}/branches/{branch}/catalog-identities/{binding}/observe-current", s.auth(http.HandlerFunc(s.catalogObserveCurrent), true))
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/default-database-owner", s.auth(http.HandlerFunc(s.defaultDatabaseOwnerRead), false))
	mux.Handle("PATCH /api/v1/projects/{project}/branches/{branch}/default-database-owner", s.auth(http.HandlerFunc(s.defaultDatabaseOwnerMutation), true))
	mux.Handle("GET /api/v1/projects/{project}/endpoints", s.auth(http.HandlerFunc(s.endpoints), false))
	mux.Handle("POST /api/v1/projects/{project}/endpoints", s.auth(http.HandlerFunc(s.createEndpoint), true))
	mux.Handle("GET /api/v1/projects/{project}/endpoints/{endpoint}", s.auth(http.HandlerFunc(s.endpoint), false))
	mux.Handle("DELETE /api/v1/projects/{project}/endpoints/{endpoint}", s.auth(http.HandlerFunc(s.deleteEndpoint), true))
	mux.Handle("GET /api/v1/projects/{project}/endpoints/{endpoint}/connection-info", s.auth(http.HandlerFunc(s.connectionInfo), false))
	mux.Handle("GET /api/v1/projects/{project}/endpoints/{endpoint}/metrics", s.auth(http.HandlerFunc(s.metrics), false))
	mux.Handle("GET /api/v1/projects/{project}/operations", s.auth(http.HandlerFunc(s.operations), false))
	mux.Handle("GET /api/v1/projects/{project}/operations/{operation}", s.auth(http.HandlerFunc(s.operation), false))
	mux.Handle("POST /api/v1/projects/{project}/operations/{operation}/retry", s.auth(http.HandlerFunc(s.retryOperation), true))
	mux.Handle("PATCH /api/v1/projects/{project}/endpoints/{endpoint}", s.auth(http.HandlerFunc(s.updateEndpoint), true))
	mux.Handle("PATCH /api/v1/projects/{project}/endpoints/{endpoint}/lifecycle", s.auth(http.HandlerFunc(s.endpointLifecycle), true))
	mux.Handle("POST /api/v1/projects/{project}/endpoints/{endpoint}/suspend", s.auth(http.HandlerFunc(s.suspendEndpoint), true))
	mux.Handle("POST /api/v1/projects/{project}/endpoints/{endpoint}/query", s.auth(http.HandlerFunc(s.querySQL), true))
	mux.Handle("POST /api/v1/projects/{project}/endpoints/{endpoint}/query-script", s.auth(http.HandlerFunc(s.querySQLScript), true))
	mux.HandleFunc("GET /api/openapi.json", s.openapi)
	mux.HandleFunc("GET /api/application-auth-openapi.json", s.applicationAuthOpenAPI)
	mux.HandleFunc("GET /api/docs", s.swagger)
	dist := env("NEON_V2_WEB_DIST", filepath.Join("..", "web", "dist"))
	mux.Handle("GET /api/docs/assets/", http.StripPrefix("/api/docs/assets/", http.FileServer(http.Dir(filepath.Join(dist, "api", "docs", "assets")))))
	if _, err := os.Stat(filepath.Join(dist, "index.html")); err == nil {
		mux.Handle("/", http.FileServer(http.Dir(dist)))
	}
	return mux
}

func (s *server) capabilities(w http.ResponseWriter, r *http.Request) {
	creation := map[string]any{"enabled": creationEnabled(), "reason": "m2_create_driver"}
	if !creationEnabled() {
		creation["reason"] = "creation_driver_disabled"
	}
	jsonResponse(w, 200, map[string]any{
		"cluster_id": "rke2-lab", "observed_at": time.Now().UTC(),
		"runtime": s.runtimeStatus(r.Context()),
		"features": map[string]any{
			"console_invitations":  map[string]any{"enabled": true, "reason": "account_bound_invitation_registration"},
			"tenant_authorization": map[string]any{"enabled": true, "reason": "organization_and_additive_project_policy"},
			"api_keys":             map[string]any{"enabled": true, "reason": "hashed_scoped_revocable_credentials"},
			"backend_credentials":  map[string]any{"enabled": s.backendKeys != nil, "reason": "branch_scoped_credentials_inference_independent"},
			"database_management":  map[string]any{"enabled": true, "reason": "branch_catalog_operation_driver"},
			"role_management":      map[string]any{"enabled": true, "reason": "branch_role_and_proxy_spec_reconciliation"},
			"sql_scripts":          map[string]any{"enabled": true, "reason": "bounded_native_postgresql_simple_protocol"},
			"project_create":       creation, "branch_create": creation, "endpoint_create": creation,
			"neonvm_autoscaling": map[string]any{"enabled": true, "reason": "validated_on_vm_lab"},
			"vm_scale_to_zero":   map[string]any{"enabled": os.Getenv("NEON_V2_SCALE_ZERO_ENABLED") == "true", "reason": "lab_idle_controller"},
			"read_replicas":      map[string]any{"enabled": creationEnabled(), "reason": "safekeeper_streaming_validated"},
			"project_delete":     map[string]any{"enabled": creationEnabled(), "reason": "retained_deletion_no_physical_gc"},
			"branch_delete":      map[string]any{"enabled": creationEnabled(), "reason": "protected_leaf_retained_deletion"},
			"endpoint_delete":    map[string]any{"enabled": creationEnabled(), "reason": "owned_compute_retirement_branch_data_retained"},
			"pitr_new_branch":    map[string]any{"enabled": pitrEnabled(), "reason": "retained_timestamp_or_lsn_new_branch"},
		},
		"services": map[string]any{"postgres": map[string]any{"enabled": true, "reason": "read_and_query_validated"},
			"auth": map[string]any{"enabled": managedAuthEnabled(), "reason": "better_auth_branch_identity_lab_transport"}, "object_storage": map[string]any{"enabled": s.storage != nil, "reason": "branch_object_rest_v1_s3_gate_pending"}, "functions": map[string]any{"enabled": s.functions != nil, "reason": "native_microvm_http_preview", "public_http_enabled": s.functions != nil && functionPublicOrigin() != "", "automatic_zero_enabled": s.functions != nil && functionPublicOrigin() != "", "production_qualified": false}, "ai_gateway": map[string]any{"enabled": s.aiEnabled(), "reason": "approved_routes_text_chat_preview", "production_qualified": false},
			"data_api": map[string]any{"enabled": dataAPIEnabled(), "reason": "native_driver_lab_transport_gate"}},
		"limits": map[string]any{"min_cpu_milli": 1000, "max_cpu_milli": 2000, "min_memory_mib": 1024, "max_memory_mib": 3072, "supported_memory_slot_mib": []int{1024}},
	})
}

func (s *server) requireProject(w http.ResponseWriter, r *http.Request) (record, bool) {
	item, err := s.one(r.Context(), "SELECT * FROM projects WHERE id=$1 AND deleted_at IS NULL", r.PathValue("project"))
	if err != nil {
		if isNoRows(err) {
			fail(w, r, 404, "not_found", "Project not found")
		} else {
			s.logger.Error("project read", "error", err)
			fail(w, r, 503, "metadata_unavailable", "Could not read project")
		}
		return nil, false
	}
	item["effective_permission"] = permissionName(accessFrom(r).Level)
	return item, true
}
func (s *server) requireEndpoint(w http.ResponseWriter, r *http.Request) (record, bool) {
	item, err := s.one(r.Context(), "SELECT * FROM endpoints WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL", r.PathValue("endpoint"), r.PathValue("project"))
	if err != nil {
		if isNoRows(err) {
			fail(w, r, 404, "not_found", "Endpoint not found")
		} else {
			s.logger.Error("endpoint read", "error", err)
			fail(w, r, 503, "metadata_unavailable", "Could not read endpoint")
		}
		return nil, false
	}
	if item["state"] == "deleting" || item["state"] == "deleted" {
		fail(w, r, 410, "resource_deleted", "Compute admission is closed")
		return nil, false
	}
	return item, true
}
func page(items []record) map[string]any { return map[string]any{"items": items, "next_cursor": nil} }

func (s *server) projects(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	trash := r.URL.Query().Get("deleted") == "true"
	if value := r.URL.Query().Get("deleted"); value != "" && value != "true" && value != "false" {
		fail(w, r, 422, "invalid_filter", "deleted must be true or false")
		return
	}
	items, err := s.many(r.Context(), `SELECT p.*,m.role AS organization_role,COALESCE(g.role,'') AS project_role
		FROM projects p JOIN organization_members m ON m.org_id=p.org_id AND m.user_id=$2
		LEFT JOIN project_grants g ON g.project_id=p.id AND g.user_id=m.user_id
		WHERE p.org_id=$1 AND ((NOT $4 AND p.deleted_at IS NULL) OR ($4 AND p.deleted_at IS NOT NULL
		AND (m.role IN ('owner','admin') OR g.role='admin'))) AND ($3='' OR p.id=$3)
		AND (m.role<>'collaborator' OR g.role IS NOT NULL) ORDER BY p.created_at DESC LIMIT 100`, r.PathValue("org"), u.ID, u.KeyProject, trash)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not list projects")
		return
	}
	visible := []record{}
	for _, item := range items {
		level := effectivePermission(stringVal(item["organization_role"]), stringVal(item["project_role"]))
		if u.APIKey {
			level = min(level, roleLevel(u.KeyRole))
		}
		if trash && level < 3 {
			continue
		}
		item["effective_permission"] = permissionName(level)
		delete(item, "organization_role")
		delete(item, "project_role")
		visible = append(visible, item)
	}
	jsonResponse(w, 200, page(visible))
}
func (s *server) project(w http.ResponseWriter, r *http.Request) {
	item, ok := s.requireProject(w, r)
	if ok {
		jsonResponse(w, 200, item)
	}
}
func (s *server) branches(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireProject(w, r); !ok {
		return
	}
	items, err := s.many(r.Context(), "SELECT * FROM branches WHERE project_id=$1 AND deleted_at IS NULL ORDER BY created_at LIMIT 100", r.PathValue("project"))
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not list branches")
		return
	}
	jsonResponse(w, 200, page(items))
}
func (s *server) branch(w http.ResponseWriter, r *http.Request) {
	item, err := s.one(r.Context(), "SELECT * FROM branches WHERE project_id=$1 AND id=$2 AND deleted_at IS NULL", r.PathValue("project"), r.PathValue("branch"))
	if err != nil {
		if isNoRows(err) {
			fail(w, r, 404, "not_found", "Branch not found")
		} else {
			fail(w, r, 503, "metadata_unavailable", "Could not read branch")
		}
		return
	}
	jsonResponse(w, 200, item)
}
func (s *server) branchServices(w http.ResponseWriter, r *http.Request) {
	branch, err := s.one(r.Context(), "SELECT id,state FROM branches WHERE project_id=$1 AND id=$2 AND deleted_at IS NULL", r.PathValue("project"), r.PathValue("branch"))
	if err != nil {
		if isNoRows(err) {
			fail(w, r, 404, "not_found", "Branch not found")
		} else {
			fail(w, r, 503, "metadata_unavailable", "Could not read branch")
		}
		return
	}
	rows, err := s.many(r.Context(), `SELECT service_kind,desired_state,observed_state,driver_version,public_endpoint,
        version,last_observed_at FROM branch_service_instances WHERE branch_id=$1`, branch["id"])
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read branch services")
		return
	}
	byKind := map[string]record{}
	for _, row := range rows {
		byKind[stringVal(row["service_kind"])] = row
	}
	items := []record{}
	for _, kind := range []string{"postgres", "auth", "object_storage", "functions", "ai_gateway", "data_api"} {
		// Approved inference routes do not allocate a per-branch Compute. Report
		// configuration separately from upstream health, instead of the legacy
		// "driver_not_implemented" placeholder after a real gateway is installed.
		if kind == "ai_gateway" && s.aiEnabled() && branch["state"] == "ready" {
			items = append(items, record{"service_kind": kind, "desired_state": "active", "observed_state": "configured", "enabled": true, "reason": "approved_routes_text_chat_preview", "public_endpoint": "/ai/v1/" + stringVal(branch["id"]) + "/v1", "driver_version": "go-ai-text-chat-v1"})
			continue
		}
		if item, ok := byKind[kind]; ok {
			item["enabled"] = (kind == "postgres" || kind == "data_api" && dataAPIEnabled() || kind == "auth" && managedAuthEnabled() || kind == "object_storage" && s.storage != nil) && item["observed_state"] == "active"
			item["reason"] = ""
			if item["enabled"] != true {
				item["reason"] = "not_ready_or_driver_not_implemented"
			}
			items = append(items, item)
		} else {
			items = append(items, record{"service_kind": kind, "desired_state": "disabled", "observed_state": "disabled",
				"enabled": false, "reason": "driver_not_implemented", "public_endpoint": nil, "driver_version": nil})
		}
	}
	jsonResponse(w, 200, map[string]any{"branch_id": branch["id"], "items": items})
}
func (s *server) decorateEndpoint(ctx context.Context, item record) record {
	runtime := s.kube.runtime(ctx, stringVal(item["workload_kind"]), stringVal(item["workload_name"]))
	item["mode"] = item["workload_kind"]
	item["runtime"] = runtime
	item["observed_state"] = runtime["observed_state"]
	item["desired_state"] = item["state"]
	return item
}
func (s *server) endpoints(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireProject(w, r); !ok {
		return
	}
	items, err := s.many(r.Context(), "SELECT * FROM endpoints WHERE project_id=$1 AND deleted_at IS NULL ORDER BY created_at", r.PathValue("project"))
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not list endpoints")
		return
	}
	for i := range items {
		defaults, err := s.publicConnectionDefaults(r.Context(), items[i])
		if err != nil {
			fail(w, r, 503, "connection_defaults_unavailable", "Could not read customer connection defaults")
			return
		}
		items[i]["connection_defaults"] = defaults
		items[i] = s.decorateEndpoint(r.Context(), items[i])
	}
	jsonResponse(w, 200, page(items))
}
func (s *server) endpoint(w http.ResponseWriter, r *http.Request) {
	item, ok := s.requireEndpoint(w, r)
	if ok {
		defaults, err := s.publicConnectionDefaults(r.Context(), item)
		if err != nil {
			fail(w, r, 503, "connection_defaults_unavailable", "Could not read customer connection defaults")
			return
		}
		item["connection_defaults"] = defaults
		jsonResponse(w, 200, s.decorateEndpoint(r.Context(), item))
	}
}

func (s *server) connectionInfo(w http.ResponseWriter, r *http.Request) {
	item, ok := s.requireEndpoint(w, r)
	if !ok {
		return
	}
	defaults, err := s.publicConnectionDefaults(r.Context(), item)
	if err != nil {
		fail(w, r, 503, "connection_defaults_unavailable", "Could not read customer connection defaults")
		return
	}
	selector, role, database := stringVal(item["selector"]), stringVal(defaults["role"]), stringVal(defaults["database"])
	// SQL uses the internal Proxy address; clients receive the public address.
	host, port := env("NEON_PUBLIC_PROXY_HOST", s.proxyHost), env("NEON_PUBLIC_PROXY_PORT", s.proxyPort)
	mode := "require"
	if os.Getenv("NEON_PG_TLS_MODE") == "verify-full" {
		mode = "verify-full"
	}
	uri := fmt.Sprintf("postgresql://%s:<PASSWORD>@%s:%s/%s?sslmode=%s&options=endpoint%%3D%s", url.QueryEscape(role), host, port, url.QueryEscape(database), mode, url.QueryEscape(selector))
	n, _ := strconv.Atoi(port)
	jsonResponse(w, 200, map[string]any{"endpoint_id": item["id"], "host": host, "port": n, "database": database, "role": role,
		"ssl_mode": mode, "endpoint_selector": selector, "connection_uri_template": uri, "password_included": false})
}

func (s *server) operationRecord(ctx context.Context, id, project string) (record, error) {
	item, err := s.one(ctx, "SELECT id,project_id,resource_type,resource_id,action,state,actor_id,request_id,error_code,error_message,retryable,attempts,created_at,started_at,finished_at FROM operations WHERE id=$1 AND project_id=$2", id, project)
	if err != nil {
		return nil, err
	}
	steps, err := s.many(ctx, "SELECT ordinal,name,state,attempts,detail,updated_at FROM operation_steps WHERE operation_id=$1 ORDER BY ordinal", id)
	if err != nil {
		return nil, err
	}
	item["steps"] = steps
	return item, nil
}
func (s *server) operations(w http.ResponseWriter, r *http.Request) {
	// The authorization middleware exposes retained project Operations only
	// to an effective Admin. Do not require a live project after deletion.
	rows, err := s.many(r.Context(), "SELECT id FROM operations WHERE project_id=$1 ORDER BY created_at DESC LIMIT 100", r.PathValue("project"))
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not list operations")
		return
	}
	items := []record{}
	for _, row := range rows {
		item, err := s.operationRecord(r.Context(), stringVal(row["id"]), r.PathValue("project"))
		if err == nil {
			items = append(items, item)
		}
	}
	jsonResponse(w, 200, page(items))
}
func (s *server) operation(w http.ResponseWriter, r *http.Request) {
	item, err := s.operationRecord(r.Context(), r.PathValue("operation"), r.PathValue("project"))
	if err != nil {
		if isNoRows(err) {
			fail(w, r, 404, "not_found", "Operation not found")
		} else {
			fail(w, r, 503, "metadata_unavailable", "Could not read operation")
		}
		return
	}
	jsonResponse(w, 200, item)
}

func readJSON(r *http.Request, out any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}
func requestHash(scope string, value any) string {
	b, _ := json.Marshal(value)
	sum := sha256.Sum256(append([]byte(scope+":"), b...))
	return hex.EncodeToString(sum[:])
}
func keyHash(key string) string { return digest(key) }

func (s *server) updateEndpoint(w http.ResponseWriter, r *http.Request) {
	item, ok := s.requireEndpoint(w, r)
	if !ok {
		return
	}
	if item["workload_kind"] != "neonvm" {
		fail(w, r, 422, "capability_disabled", "Autoscaling bounds require NeonVM")
		return
	}
	var body struct {
		Autoscaling struct {
			MinCPU int `json:"min_cpu_milli"`
			MaxCPU int `json:"max_cpu_milli"`
			MinMem int `json:"min_memory_mib"`
			MaxMem int `json:"max_memory_mib"`
		} `json:"autoscaling"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, r, 422, "invalid_request", err.Error())
		return
	}
	a := body.Autoscaling
	if !(createBounds{a.MinCPU, a.MaxCPU, a.MinMem, a.MaxMem}).valid() {
		fail(w, r, 422, "invalid_scaling_bounds", "Bounds must fit VM capacity and memory slots")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 {
		fail(w, r, 422, "invalid_idempotency_key", "Idempotency-Key required")
		return
	}
	scope := "PATCH:/projects/" + r.PathValue("project") + "/endpoints/" + r.PathValue("endpoint")
	hash := requestHash(scope, body)
	u := userFrom(r)
	var existingHash, existingOp string
	err := s.db.QueryRow(r.Context(), "SELECT request_hash,operation_id FROM idempotency_keys WHERE actor_id=$1 AND key_hash=$2", u.ID, keyHash(key)).Scan(&existingHash, &existingOp)
	if err == nil {
		if existingHash != hash {
			fail(w, r, 409, "idempotency_conflict", "Key used with different input")
			return
		}
		op, err := s.operationRecord(r.Context(), existingOp, r.PathValue("project"))
		if err != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not replay operation")
			return
		}
		jsonResponse(w, 202, map[string]any{"resource": item, "operation": op})
		return
	}
	if err != nil && !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not check idempotency")
		return
	}
	if r.Header.Get("If-Match") != fmt.Sprintf("\"%v\"", item["version"]) {
		fail(w, r, 412, "version_mismatch", "Reload endpoint before editing")
		return
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Transaction unavailable")
		return
	}
	defer tx.Rollback(r.Context())
	var version int64
	err = tx.QueryRow(r.Context(), "SELECT version FROM endpoints WHERE id=$1 AND project_id=$2 FOR UPDATE", r.PathValue("endpoint"), r.PathValue("project")).Scan(&version)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Endpoint lock failed")
		return
	}
	if r.Header.Get("If-Match") != fmt.Sprintf("\"%d\"", version) {
		fail(w, r, 412, "version_mismatch", "Reload endpoint before editing")
		return
	}
	var pending string
	err = tx.QueryRow(r.Context(), "SELECT id FROM operations WHERE resource_type='endpoint' AND resource_id=$1 AND state IN ('queued','running','retry_wait') LIMIT 1", r.PathValue("endpoint")).Scan(&pending)
	if err == nil {
		fail(w, r, 409, "endpoint_operation_in_progress", "Endpoint operation in progress")
		return
	}
	if !isNoRows(err) {
		fail(w, r, 503, "metadata_unavailable", "Could not check endpoint operations")
		return
	}
	opID := newID("op_")
	payload, _ := json.Marshal(map[string]any{"project_id": item["project_id"], "endpoint_id": item["id"], "workload_name": item["workload_name"],
		"min_cpu_milli": a.MinCPU, "max_cpu_milli": a.MaxCPU, "min_memory_mib": a.MinMem, "max_memory_mib": a.MaxMem})
	_, err = tx.Exec(r.Context(), `INSERT INTO operations(id,project_id,resource_type,resource_id,action,state,actor_id,request_id,payload)
        VALUES($1,$2,'endpoint',$3,'update_endpoint','queued',$4,$5,$6)`, opID, r.PathValue("project"), r.PathValue("endpoint"), u.ID, requestID(r), payload)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO operation_steps(operation_id,ordinal,name,state) VALUES
        ($1,0,'patch_neonvm_bounds','queued'),($1,1,'persist_endpoint_metadata','queued')`, opID)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO idempotency_keys(actor_id,key_hash,request_hash,operation_id) VALUES($1,$2,$3,$4)", u.ID, keyHash(key), hash, opID)
	}
	if err != nil {
		fail(w, r, 409, "operation_conflict", "Could not queue endpoint operation")
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not commit operation")
		return
	}
	s.audit(r.Context(), u.ID, "update_endpoint", "endpoint", r.PathValue("endpoint"), "accepted", requestID(r))
	op, _ := s.operationRecord(r.Context(), opID, r.PathValue("project"))
	jsonResponse(w, 202, map[string]any{"resource": item, "operation": op})
}

func (s *server) openapi(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, env("NEON_V2_OPENAPI", filepath.Join("..", "contracts", "openapi-v1.json")))
}
func (s *server) swagger(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8"><title>Neon Control API</title><link rel="stylesheet" href="/api/docs/assets/swagger-ui.css"><link rel="icon" href="/api/docs/assets/favicon-32x32.png"></head><body><div id="swagger-ui"></div><script src="/api/docs/assets/swagger-ui-bundle.js"></script><script src="/api/docs/assets/swagger-ui-standalone-preset.js"></script><script src="/api/docs/assets/swagger-init.js"></script></body></html>`))
}

func (s *server) metrics(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireEndpoint(w, r); !ok {
		return
	}
	periods := map[string]time.Duration{"15m": 15 * time.Minute, "1h": time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour}
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "1h"
	}
	window, ok := periods[period]
	if !ok {
		fail(w, r, 422, "invalid_period", "period must be 15m, 1h, 6h or 24h")
		return
	}
	items, err := s.many(r.Context(), `SELECT endpoint_id,sampled_at,observed_state,cpu_allocated_milli,cpu_used_milli,
        memory_allocated_mib,memory_used_mib,connections,active_connections,idle_connections,database_size_bytes,
        deadlocks_total,rows_inserted_total,rows_updated_total,rows_deleted_total,errors
        FROM metric_samples WHERE endpoint_id=$1 AND sampled_at > $2 ORDER BY sampled_at`, r.PathValue("endpoint"), time.Now().Add(-window))
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not load metrics")
		return
	}
	var age any
	fresh := false
	if len(items) > 0 {
		if t, ok := items[len(items)-1]["sampled_at"].(time.Time); ok {
			seconds := int(time.Since(t).Seconds())
			age = seconds
			fresh = seconds <= 90
		}
	}
	jsonResponse(w, 200, map[string]any{"endpoint_id": r.PathValue("endpoint"), "period": period,
		"sample_interval_seconds": 30, "retention_days": 7, "sample_count": len(items), "latest_age_seconds": age, "fresh": fresh,
		"sources": map[string]string{"allocation": "NeonVM Kubernetes CRD", "usage": "metrics-server / runner Pod", "postgres": "pg_stat_database / pg_stat_activity"}, "items": items})
}

func (s *server) querySQL(w http.ResponseWriter, r *http.Request) {
	item, ok := s.requireEndpoint(w, r)
	if !ok {
		return
	}
	var body struct {
		Password string `json:"password"`
		SQL      string `json:"sql"`
		Role     string `json:"role,omitempty"`
		Database string `json:"database,omitempty"`
	}
	if err := readJSON(r, &body); err != nil || body.Password == "" || len(body.SQL) == 0 || len(body.SQL) > 32000 {
		fail(w, r, 422, "invalid_request", "Password and SQL required")
		return
	}
	role, database := body.Role, body.Database
	defaults, err := s.publicConnectionDefaults(r.Context(), item)
	if err != nil {
		fail(w, r, 503, "connection_defaults_unavailable", "SQL was not submitted because customer connection defaults could not be read")
		return
	}
	if role == "" {
		role = stringVal(defaults["role"])
	}
	if database == "" {
		database = stringVal(defaults["database"])
	}
	if !validPGIdentifier(role) || !validPGIdentifier(database) ||
		(role != "cloud_admin" && protectedPGRole(role)) {
		fail(w, r, 422, "invalid_connection_target", "Choose a valid public database role and database")
		return
	}
	history, historyErr := s.beginSQLHistory(r, item, "single", role, database, body.SQL)
	if historyErr != nil {
		fail(w, r, 503, "history_unavailable", "SQL was not submitted because its history admission could not be recorded")
		return
	}
	result, err := runSQL(r.Context(), s.proxyHost, s.proxyPort, role, body.Password,
		database, stringVal(item["selector"]), body.SQL)
	if err != nil {
		outcome, state := sqlHistoryFailure(err)
		history.finish(w, outcome, state)
		s.sqlFailureResponse(w, r, err)
		return
	}
	history.finish(w, "completed", "")
	s.audit(r.Context(), userFrom(r).ID, "execute_sql", "endpoint", r.PathValue("endpoint"), "succeeded", requestID(r))
	jsonResponse(w, 200, result)
}

func (s *server) sqlFailureResponse(w http.ResponseWriter, r *http.Request, err error) {
	failure := classifySQLFailure(err)
	// Allow only bounded identifiers and classification fields. Raw driver
	// errors may contain passwords, SQL, DSNs or PostgreSQL user data.
	s.logger.Warn("SQL request failed", "request_id", requestID(r),
		"project_id", r.PathValue("project"), "endpoint_id", r.PathValue("endpoint"),
		"stage", failure.stage, "error_class", failure.class,
		"sqlstate", failure.sqlState, "status", failure.status, "retryable", failure.retryable)
	jsonResponse(w, failure.status, apiError{failure.code, failure.message, requestID(r), failure.retryable})
}

func safeSQLError(err error) string {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		message := postgresError.Message
		if len(message) > 240 {
			message = message[:240]
		}
		return message
	}
	return "Database connection or query failed; inspect the request ID in server logs"
}
