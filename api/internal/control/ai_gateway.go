package control

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

type aiTicket struct {
	Actor      string `json:"actor"`
	Branch     string `json:"branch"`
	Model      string `json:"model"`
	ID         string `json:"id"`
	Hash       string `json:"hash"`
	Version    string `json:"version"`
	Expires    int64  `json:"expires"`
	Generation int    `json:"generation,omitempty"`
}

func (s *server) aiEnabled() bool {
	return s.ai != nil && s.backendKeys != nil && os.Getenv("NEON_AI_GATEWAY_URL") != ""
}

func (s *server) aiRelay(w http.ResponseWriter, r *http.Request) {
	base := os.Getenv("NEON_AI_GATEWAY_URL")
	if s.ai == nil || base == "" {
		aigateway.Error(w, 503, "ai_gateway_disabled")
		return
	}
	path := "/models"
	if r.Method == http.MethodPost {
		path = "/chat/completions"
	}
	request, e := http.NewRequestWithContext(r.Context(), r.Method, strings.TrimRight(base, "/")+"/ai/v1/"+url.PathEscape(r.PathValue("aiBranch"))+"/v1"+path, io.LimitReader(r.Body, aigateway.MaxBodyBytes+1))
	if e != nil {
		aigateway.Error(w, 503, "ai_gateway_disabled")
		return
	}
	request.Header.Set("Authorization", r.Header.Get("Authorization"))
	request.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{Proxy: nil, ResponseHeaderTimeout: time.Duration(s.ai.TimeoutSeconds) * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Duration(s.ai.TimeoutSeconds+5) * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, e := client.Do(request)
	if e != nil {
		aigateway.Error(w, 502, "gateway_unavailable")
		return
	}
	defer response.Body.Close()
	for _, name := range []string{"Content-Type", "Cache-Control", "X-Request-ID", "X-Accel-Buffering"} {
		if v := response.Header.Get(name); v != "" {
			w.Header().Set(name, v)
		}
	}
	w.WriteHeader(response.StatusCode)
	controller := http.NewResponseController(w)
	buf := make([]byte, 4096)
	reader := io.LimitReader(response.Body, aigateway.MaxResponseBytes+4096)
	for {
		n, e := reader.Read(buf)
		if n > 0 {
			if _, e := w.Write(buf[:n]); e != nil {
				return
			}
			if controller.Flush() != nil {
				return
			}
		}
		if e != nil {
			return
		}
	}
}
func (s *server) signAITicket(actor, branch, model string, raw []byte) string {
	return s.signAITicketFor(actor, branch, model, raw, s.ai.Generation)
}
func (s *server) signAITicketFor(actor, branch, model string, raw []byte, generation int) string {
	t := aiTicket{Actor: actor, Branch: branch, Model: model, ID: newID("air_"), Hash: digest(string(raw)), Version: s.backendKeys.Active, Expires: time.Now().Add(30 * time.Second).Unix(), Generation: generation}
	b, _ := json.Marshal(t)
	value := base64.RawURLEncoding.EncodeToString(b)
	return value + "." + s.backendHash("ai-console-ticket", value, t.Version)
}
func (s *server) verifyAITicket(raw string, body []byte, branch, model string) (aiTicket, error) {
	var t aiTicket
	p := strings.Split(raw, ".")
	if len(raw) > 2048 || len(p) != 2 {
		return t, errBackendCredentialInvalid
	}
	b, e := base64.RawURLEncoding.DecodeString(p[0])
	if e != nil || json.Unmarshal(b, &t) != nil || s.backendKeys.decoded[t.Version] == nil {
		return t, errBackendCredentialInvalid
	}
	if !hmac.Equal([]byte(p[1]), []byte(s.backendHash("ai-console-ticket", p[0], t.Version))) || t.Expires < time.Now().Unix() || t.Expires > time.Now().Add(30*time.Second).Unix() || t.Hash != digest(string(body)) || t.Branch != branch || t.Model != model || !strings.HasPrefix(t.ID, "air_") {
		return t, errBackendCredentialInvalid
	}
	return t, nil
}

func loadAIConfig() (*aigateway.Config, error) {
	path := os.Getenv("NEON_AI_CONFIG_FILE")
	if path == "" {
		return nil, nil
	}
	return aigateway.LoadPublic(path)
}
func (s *server) aiRead(w http.ResponseWriter, r *http.Request) {
	branch, project := r.PathValue("branch"), r.PathValue("project")
	if _, e := s.one(r.Context(), `SELECT id FROM branches WHERE id=$1 AND project_id=$2 AND state='ready' AND deleted_at IS NULL`, branch, project); e != nil {
		fail(w, r, 404, "not_found", "Branch not found")
		return
	}
	if strings.HasSuffix(r.URL.Path, "/usage") {
		items, e := s.many(r.Context(), `SELECT id,model_id,streaming,state,started_at,finished_at,prompt_tokens,completion_tokens,total_tokens,duration_ms,first_token_ms FROM ai_gateway_requests WHERE project_id=$1 AND branch_id=$2 ORDER BY started_at DESC LIMIT 50`, project, branch)
		if e != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not read AI usage")
			return
		}
		summary, e := s.one(r.Context(), `SELECT count(*) AS requests,count(*) FILTER(WHERE state='completed') AS completed,count(*) FILTER(WHERE total_tokens IS NULL) AS unknown_usage,COALESCE(sum(total_tokens),0) AS known_total_tokens FROM ai_gateway_requests WHERE project_id=$1 AND branch_id=$2 AND started_at>now()-interval '24 hours'`, project, branch)
		if e != nil {
			fail(w, r, 503, "metadata_unavailable", "Could not read AI usage")
			return
		}
		jsonResponse(w, 200, record{"items": items, "summary": summary, "period": "24h", "prompts_recorded": false, "billing_enabled": false})
		return
	}
	enabled := s.ai != nil && s.backendKeys != nil && os.Getenv("NEON_AI_GATEWAY_URL") != ""
	models := []map[string]any{}
	if enabled {
		snapshot, err := s.aiCatalog(r.Context(), false)
		if err != nil {
			fail(w, r, 503, "provider_catalog_unavailable", "Could not read published model routes")
			return
		}
		models = snapshot.Config.PublicModels()
	}
	jsonResponse(w, 200, record{"enabled": enabled, "models": models, "public_endpoint": "/ai/v1/" + branch + "/v1", "supported_protocols": []string{"chat_completions"}, "billing_enabled": false, "reason": "operator_routes_text_chat_preview"})
}
func (s *server) aiConsoleRequest(w http.ResponseWriter, r *http.Request) {
	if s.ai == nil || s.backendKeys == nil || os.Getenv("NEON_AI_GATEWAY_URL") == "" {
		fail(w, r, 503, "ai_gateway_disabled", "AI Gateway is not configured")
		return
	}
	if userFrom(r).APIKey {
		fail(w, r, 403, "console_session_required", "Playground requires a Console session")
		return
	}
	raw, e := io.ReadAll(io.LimitReader(r.Body, aigateway.MaxBodyBytes+1))
	if e != nil {
		fail(w, r, 422, "invalid_request", "Invalid chat request")
		return
	}
	snapshot, e := s.aiCatalog(r.Context(), false)
	if e != nil {
		fail(w, r, 503, "provider_catalog_unavailable", "Could not read published model routes")
		return
	}
	q, _, e := aigateway.Decode(raw, snapshot.Config)
	if e != nil {
		fail(w, r, 422, "invalid_request", e.Error())
		return
	}
	branch := r.PathValue("branch")
	if _, e = s.one(r.Context(), `SELECT id FROM branches WHERE id=$1 AND project_id=$2 AND state='ready' AND deleted_at IS NULL`, branch, r.PathValue("project")); e != nil {
		fail(w, r, 404, "not_found", "Branch not found")
		return
	}
	base, e := url.Parse(os.Getenv("NEON_AI_GATEWAY_URL"))
	if e != nil || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Scheme != "http" && base.Scheme != "https") {
		fail(w, r, 503, "ai_gateway_disabled", "Invalid gateway configuration")
		return
	}
	upstream, e := http.NewRequestWithContext(r.Context(), "POST", strings.TrimRight(base.String(), "/")+"/ai/v1/"+url.PathEscape(branch)+"/v1/chat/completions", bytes.NewReader(raw))
	if e != nil {
		fail(w, r, 503, "ai_gateway_disabled", "Invalid gateway configuration")
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("X-Neon-Console-Ticket", s.signAITicketFor(userFrom(r).ID, branch, q.Model, raw, snapshot.Config.Generation))
	transport := &http.Transport{Proxy: nil, ResponseHeaderTimeout: time.Duration(s.ai.TimeoutSeconds) * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Duration(s.ai.TimeoutSeconds+5) * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, e := client.Do(upstream)
	if e != nil {
		fail(w, r, 502, "gateway_unavailable", "Inference outcome unavailable; request was not replayed")
		return
	}
	defer response.Body.Close()
	for _, name := range []string{"Content-Type", "Cache-Control", "X-Request-ID", "X-Accel-Buffering"} {
		if v := response.Header.Get(name); v != "" {
			w.Header().Set(name, v)
		}
	}
	w.WriteHeader(response.StatusCode)
	controller := http.NewResponseController(w)
	buf := make([]byte, 4096)
	reader := io.LimitReader(response.Body, aigateway.MaxResponseBytes+4096)
	for {
		n, e := reader.Read(buf)
		if n > 0 {
			if _, e := w.Write(buf[:n]); e != nil {
				return
			}
			if q.Stream && controller.Flush() != nil {
				return
			}
		}
		if e != nil {
			return
		}
	}
}

// RunAIGateway is an independent process: no controllers, migrations, Kubernetes
// client, database Compute startup, Console cookies or provider credentials in logs.
func RunAIGateway(ctx context.Context) error {
	cfg, e := aigateway.Load(os.Getenv("NEON_AI_CONFIG_FILE"))
	if e != nil || cfg == nil {
		return errors.New("AI Gateway requires approved routes")
	}
	keys, e := loadBackendKeyring()
	if e != nil || keys == nil {
		return errors.New("AI Gateway requires credential keyring")
	}
	dsn := os.Getenv("NEON_AI_DATABASE_URL")
	if path := os.Getenv("NEON_AI_DATABASE_URL_FILE"); path != "" {
		b, e := os.ReadFile(path)
		if e != nil {
			return errors.New("AI metadata credential unavailable")
		}
		dsn = strings.TrimSpace(string(b))
	}
	if dsn == "" {
		return errors.New("AI metadata credential required")
	}
	pool, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		return errors.New("invalid AI metadata configuration")
	}
	pool.MaxConns = 4
	db, e := pgxpool.NewWithConfig(ctx, pool)
	if e != nil {
		return errors.New("AI metadata unavailable")
	}
	defer db.Close()
	if e = db.Ping(ctx); e != nil {
		return errors.New("AI metadata unavailable")
	}
	providers, e := loadAIProviderSettings(true)
	if e != nil {
		return e
	}
	s := &server{db: db, backendKeys: keys, ai: cfg, aiProviders: providers}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { jsonResponse(w, 200, record{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if db.Ping(ctx) != nil {
			w.WriteHeader(503)
			return
		}
		jsonResponse(w, 200, record{"status": "ready"})
	})
	mux.HandleFunc("GET /ai/v1/{branch}/v1/models", s.aiPublic)
	mux.HandleFunc("POST /ai/v1/{branch}/v1/chat/completions", s.aiPublic)
	httpServer := &http.Server{Addr: env("NEON_AI_BIND", ":8791"), Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: time.Duration(cfg.TimeoutSeconds+10) * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	done := make(chan error, 1)
	go func() { done <- httpServer.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdown)
	case e := <-done:
		if errors.Is(e, http.ErrServerClosed) {
			return nil
		}
		return errors.New("AI Gateway listener failed")
	}
}

func (s *server) aiPublic(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	snapshot, catalogErr := s.aiCatalog(r.Context(), true)
	if catalogErr != nil || snapshot.Config == nil {
		aigateway.Error(w, 503, "provider_catalog_unavailable")
		return
	}
	cfg := snapshot.Config
	branch := r.PathValue("branch")
	model := ""
	var q aigateway.ChatRequest
	var route aigateway.Route
	raw := []byte{}
	var e error
	if r.Method == http.MethodPost {
		raw, e = io.ReadAll(io.LimitReader(r.Body, aigateway.MaxBodyBytes+1))
		if e != nil {
			aigateway.Error(w, 400, "invalid_request")
			return
		}
		q, route, e = aigateway.Decode(raw, cfg)
		if e != nil {
			aigateway.Error(w, 400, "invalid_request")
			return
		}
		model = q.Model
	}
	requestID := newID("air_")
	actor, project, credential := "", "", ""
	models := []string{}
	if ticket := r.Header.Get("X-Neon-Console-Ticket"); ticket != "" && r.Method == http.MethodPost && r.Header.Get("Authorization") == "" {
		t, e := s.verifyAITicket(ticket, raw, branch, model)
		if e != nil {
			aigateway.Error(w, 401, "invalid_console_ticket")
			return
		}
		if s.aiProviders != nil && t.Generation != cfg.Generation {
			aigateway.Error(w, 409, "provider_catalog_changed")
			return
		}
		actor = t.Actor
		requestID = t.ID
		e = s.db.QueryRow(r.Context(), `SELECT b.project_id FROM branches b JOIN users u ON u.id=$2 AND NOT u.disabled JOIN projects p ON p.id=b.project_id AND p.state='ready' AND p.deleted_at IS NULL WHERE b.id=$1 AND b.state='ready' AND b.deleted_at IS NULL`, branch, actor).Scan(&project)
		a, ae := s.projectAccess(r.Context(), project, actor)
		if e != nil || ae != nil || a.Level < 2 {
			aigateway.Error(w, 403, "credential_scope_denied")
			return
		}
	} else {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			aigateway.Error(w, 401, "invalid_backend_credential")
			return
		}
		verified, e := s.verifyBackendCredential(r.Context(), strings.TrimPrefix(header, "Bearer "), branch, "ai_gateway:invoke", model)
		if errors.Is(e, errBackendCredentialForbidden) {
			aigateway.Error(w, 403, "credential_scope_denied")
			return
		}
		if e != nil {
			status := 503
			if errors.Is(e, errBackendCredentialInvalid) {
				status = 401
			}
			aigateway.Error(w, status, "credential_unavailable")
			return
		}
		credential = stringVal(verified["credential_id"])
		if s.db.QueryRow(r.Context(), `SELECT issuer_id,project_id,allowed_models FROM backend_credentials WHERE id=$1`, credential).Scan(&actor, &project, &models) != nil {
			aigateway.Error(w, 503, "credential_unavailable")
			return
		}
	}
	if r.Method == http.MethodGet {
		allowed := cfg.PublicModels()
		if len(models) > 0 {
			filtered := []map[string]any{}
			for _, m := range allowed {
				for _, id := range models {
					if m["id"] == id {
						filtered = append(filtered, m)
						break
					}
				}
			}
			allowed = filtered
		}
		jsonResponse(w, 200, record{"object": "list", "data": allowed})
		return
	}
	if snapshot.Unavailable[model] {
		aigateway.Error(w, 503, "provider_credential_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	admitted, e := s.reserveAIRequestFor(ctx, requestID, project, branch, actor, credential, q, cfg)
	if e != nil {
		if errors.Is(e, errAIProviderCatalogChanged) {
			aigateway.Error(w, 409, "provider_catalog_changed")
			return
		}
		aigateway.Error(w, 503, "request_ledger_unavailable")
		return
	}
	if !admitted {
		w.Header().Set("Retry-After", "60")
		aigateway.Error(w, 429, "branch_limit_exceeded")
		return
	}
	w.Header().Set("X-Request-ID", requestID)
	started := time.Now()
	outcome := aigateway.Forward(ctx, w, q, route, time.Duration(cfg.TimeoutSeconds)*time.Second)
	// A disconnected caller must not cancel settlement. On DB failure, preserve
	// the running reservation for operator recovery; never infer zero token usage.
	settle, close := context.WithTimeout(context.Background(), 5*time.Second)
	defer close()
	var prompt, completion, total any
	if outcome.Usage != nil {
		prompt = outcome.Usage.PromptTokens
		completion = outcome.Usage.CompletionTokens
		total = outcome.Usage.TotalTokens
	}
	if _, err := s.db.Exec(settle, `UPDATE ai_gateway_requests SET state=$2,finished_at=clock_timestamp(),prompt_tokens=$3,completion_tokens=$4,total_tokens=$5,duration_ms=$6,first_token_ms=$7,upstream_status=$8 WHERE id=$1 AND state='running'`, requestID, outcome.Status, prompt, completion, total, time.Since(started).Milliseconds(), outcome.FirstTokenMillis, outcome.UpstreamStatus); err != nil {
		slog.Warn("AI request settlement unavailable; preserve reservation", "request_id", requestID)
	}
}
func (s *server) reserveAIRequest(ctx context.Context, id, project, branch, actor, credential string, q aigateway.ChatRequest) (bool, error) {
	return s.reserveAIRequestFor(ctx, id, project, branch, actor, credential, q, s.ai)
}
func (s *server) reserveAIRequestFor(ctx context.Context, id, project, branch, actor, credential string, q aigateway.ChatRequest, cfg *aigateway.Config) (bool, error) {
	tx, e := s.db.Begin(ctx)
	if e != nil {
		return false, e
	}
	defer tx.Rollback(ctx)
	if s.aiProviders != nil {
		var revision int
		if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(794210047)`); e != nil {
			return false, e
		}
		if e = tx.QueryRow(ctx, `SELECT revision FROM ai_provider_catalog_state WHERE singleton`).Scan(&revision); e != nil {
			return false, e
		}
		if cfg.Generation != s.ai.Generation+revision {
			return false, errAIProviderCatalogChanged
		}
	}
	hash := sha256.Sum256([]byte("ai-branch:" + branch))
	lock := hex.EncodeToString(hash[:])
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lock); e != nil {
		return false, e
	}
	var active, recent int
	if e = tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE state='running' AND expires_at>clock_timestamp()),count(*) FILTER(WHERE started_at>clock_timestamp()-interval '1 minute') FROM ai_gateway_requests WHERE branch_id=$1 AND started_at>clock_timestamp()-interval '5 minutes'`, branch).Scan(&active, &recent); e != nil {
		return false, e
	}
	if active >= cfg.MaxConcurrentPerBranch || recent >= cfg.RequestsPerMinute {
		return false, nil
	}
	var cid any
	if credential != "" {
		cid = credential
	}
	_, e = tx.Exec(ctx, `INSERT INTO ai_gateway_requests(id,project_id,branch_id,actor_id,credential_id,model_id,route_generation,streaming,state,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'running',clock_timestamp()+make_interval(secs=>$9))`, id, project, branch, actor, cid, q.Model, cfg.Generation, q.Stream, cfg.TimeoutSeconds+10)
	if e != nil {
		return false, e
	}
	return true, tx.Commit(ctx)
}
