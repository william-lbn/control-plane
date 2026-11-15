package control

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/william-lbn/control-plane/api/internal/realtime"
)

const subscribeRealtime = "realtime:subscribe"

var realtimeQueryName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,47}$`)

type registeredRealtimeQuery struct {
	Source                                realtime.SourceKey
	ID, Writer, Credential, CredentialUID string
	Version                               int64
	RelationNames                         []realtime.Relation
	ParameterCount                        int
	Query                                 realtime.Query
}

type realtimeRuntime struct {
	notifications *realtime.Notifications
	openWriter    func(context.Context, registeredRealtimeQuery) (*pgx.Conn, error)
}

func (s *server) configureRealtime(ctx context.Context) error {
	if os.Getenv("NEON_REALTIME_ENABLED") != "true" {
		return nil
	}
	limit, err := strconv.Atoi(os.Getenv("NEON_REALTIME_SUBSCRIPTION_LIMIT"))
	if err != nil || limit <= 0 || s.backendKeys == nil || len(s.idempotencyKey) < sha256.Size {
		return errors.New("Realtime requires an explicit subscription budget and Backend credential keys")
	}
	n := realtime.NewNotifications(s.db, limit)
	s.realtime = &realtimeRuntime{notifications: n, openWriter: s.openRealtimeWriter}
	go func() {
		if err := n.Run(ctx, time.Second); err != nil && ctx.Err() == nil {
			s.logger.Error("Realtime notification listener exited")
		}
	}()
	return nil
}

func (s *server) readRealtimeQuery(ctx context.Context, branch, name string) (registeredRealtimeQuery, error) {
	var q registeredRealtimeQuery
	var timeout int
	err := s.db.QueryRow(ctx, `
SELECT q.id,q.version,q.writer_id,q.credential_ref,q.credential_uid,s.id,s.project_id,s.branch_id,s.timeline_id,s.database_name,
       q.statement,q.read_role,q.relations,q.parameter_count,q.max_rows,q.max_result_bytes,q.max_frame_bytes,q.query_timeout_ms
FROM realtime_queries q JOIN realtime_sources s ON s.id=q.source_id
JOIN projects p ON p.id=s.project_id
JOIN branches b ON b.id=s.branch_id AND b.project_id=s.project_id AND b.timeline_id=s.timeline_id
JOIN endpoints e ON e.id=q.writer_id AND e.project_id=q.project_id AND e.branch_id=q.branch_id
WHERE q.branch_id=$1 AND q.name=$2 AND q.retired_at IS NULL AND s.retired_at IS NULL
 AND p.state='ready' AND p.deleted_at IS NULL AND b.state='ready' AND b.deleted_at IS NULL
 AND e.state='active' AND e.deleted_at IS NULL AND e.endpoint_type='read_write'
 AND jsonb_array_length(q.relations)>0 AND q.relations <@ s.relations`, branch, name).
		Scan(&q.ID, &q.Version, &q.Writer, &q.Credential, &q.CredentialUID, &q.Source.ID,
			&q.Source.Project, &q.Source.Branch, &q.Source.Timeline, &q.Source.Database,
			&q.Query.Statement, &q.Query.Role, &q.RelationNames, &q.ParameterCount,
			&q.Query.MaxRows, &q.Query.MaxBytes, &q.Query.MaxFrameBytes, &timeout)
	q.Query.Timeout = time.Duration(timeout) * time.Millisecond
	return q, err
}

func (s *server) openRealtimeWriter(ctx context.Context, q registeredRealtimeQuery) (*pgx.Conn, error) {
	source, err := s.dsgaEndpointRuntime(ctx, q.Source.Project, q.Writer)
	if err != nil || source == nil || source.Payload.BranchID != q.Source.Branch ||
		source.Payload.TimelineID != q.Source.Timeline || source.Payload.EndpointType != "read_write" {
		return nil, realtime.ErrSourceUnavailable
	}
	_, vm, _, _, err := s.readDSGABoundSelection(ctx, source)
	if err != nil || vm == nil {
		return nil, realtime.ErrSourceUnavailable
	}
	secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", q.Credential), nil)
	if err != nil || !owned(secret, q.Source.Project, q.Writer) || nested(secret, "metadata", "uid") != q.CredentialUID ||
		nested(secret, "metadata", "labels", "neon-control/branch-id") != q.Source.Branch ||
		nested(secret, "metadata", "labels", "neon-control/realtime-query") != q.ID || secret["immutable"] != true {
		return nil, realtime.ErrSourceUnavailable
	}
	user, err := secretText(secret, "user")
	if err != nil || user != q.Query.Role {
		return nil, realtime.ErrSourceUnavailable
	}
	password, err := secretText(secret, "password")
	if err != nil || password == "" {
		return nil, realtime.ErrSourceUnavailable
	}
	conn, err := connectSQL(ctx, s.proxyHost, s.proxyPort, user, password, q.Source.Database, selector(q.Writer))
	if err != nil {
		return nil, realtime.ErrSourceUnavailable
	}
	var tenant, timeline string
	err = conn.QueryRow(ctx, `SELECT current_setting('neon.tenant_id'),current_setting('neon.timeline_id')`).Scan(&tenant, &timeline)
	if err != nil || tenant != source.Payload.TenantID || timeline != q.Source.Timeline {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = conn.Close(cleanup)
		return nil, realtime.ErrSourceUnavailable
	}
	return conn, nil
}

func (s *server) realtimeStream(w http.ResponseWriter, r *http.Request) {
	if s.realtime == nil {
		fail(w, r, 503, "realtime_unavailable", "Realtime is unavailable")
		return
	}
	branch, name := r.PathValue("rtBranch"), r.PathValue("rtQuery")
	if !dataBranchID.MatchString(branch) || !realtimeQueryName.MatchString(name) {
		fail(w, r, 404, "not_found", "Realtime query not found")
		return
	}
	token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !bearer {
		fail(w, r, 401, "invalid_backend_credential", "Realtime requires a Backend credential")
		return
	}
	actor, err := s.verifyBackendCredential(r.Context(), token, branch, subscribeRealtime, "")
	if err != nil {
		code := http.StatusServiceUnavailable
		if errors.Is(err, errBackendCredentialInvalid) {
			code = http.StatusUnauthorized
		} else if errors.Is(err, errBackendCredentialForbidden) {
			code = http.StatusForbidden
		}
		fail(w, r, code, "realtime_unavailable", "Realtime authorization is unavailable")
		return
	}
	q, err := s.readRealtimeQuery(r.Context(), branch, name)
	if err != nil {
		if isNoRows(err) {
			fail(w, r, 404, "not_found", "Realtime query not found")
		} else {
			fail(w, r, 503, "metadata_unavailable", "Realtime query unavailable")
		}
		return
	}
	args := r.URL.Query()["arg"]
	if len(args) != q.ParameterCount {
		fail(w, r, 422, "invalid_parameters", "Realtime query parameters differ")
		return
	}
	for _, arg := range args {
		if len(arg) > 4096 || strings.ContainsRune(arg, 0) {
			fail(w, r, 422, "invalid_parameters", "Realtime query parameter exceeds its limit")
			return
		}
		q.Query.Params = append(q.Query.Params, arg)
	}
	canonical, _ := json.Marshal(args)
	expires, validExpiry := actor["expires_at"].(time.Time)
	generation, validGeneration := actor["generation"].(int64)
	if !validExpiry || !validGeneration {
		fail(w, r, 503, "metadata_unavailable", "Realtime authorization is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	options := realtime.SSEOptions{
		Cursor: realtime.CursorSigner{Key: s.idempotencyKey}, ExpiresAt: expires, CursorTTL: 15 * time.Minute, Heartbeat: 15 * time.Second,
		Scope: realtime.CursorScope{Source: q.Source, Query: q.ID, QueryVersion: q.Version, Principal: stringVal(actor["credential_id"]),
			Authorization: generation, ParametersSHA256: sha256.Sum256(canonical)},
	}
	authorize := func(call context.Context) error {
		check, stop := context.WithTimeout(call, 5*time.Second)
		defer stop()
		current, err := s.verifyBackendCredential(check, token, branch, subscribeRealtime, "")
		if err != nil || current["generation"] != actor["generation"] || current["credential_id"] != actor["credential_id"] {
			return realtime.ErrSourceUnavailable
		}
		live, err := s.readRealtimeQuery(check, branch, name)
		if err != nil || live.ID != q.ID || live.Source != q.Source || live.Version != q.Version ||
			live.CredentialUID != q.CredentialUID || live.Writer != q.Writer {
			return realtime.ErrSourceUnavailable
		}
		return nil
	}
	(realtime.Store{DB: s.db}).ServeSSE(w, r.WithContext(ctx), s.realtime.notifications,
		realtime.Subscription{Key: q.Source, Query: q.Query, Relations: q.RelationNames, PageSize: 16, DeliveryLimit: 5 * time.Second},
		options, authorize, func(ctx context.Context) (*pgx.Conn, error) { return s.realtime.openWriter(ctx, q) })
}
