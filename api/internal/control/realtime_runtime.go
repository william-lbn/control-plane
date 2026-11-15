package control

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/william-lbn/control-plane/api/internal/realtime"
)

type realtimeLoginBinding struct {
	key                                     realtime.SourceKey
	writer, role, credential, credentialUID string
	ownerLabel, ownerID                     string
	maxWALBytes                             int64
}

type verifiedRealtimeConnection struct {
	config   *pgx.ConnConfig
	timeout  time.Duration
	tenantID string
}

func (s *server) realtimeLoginConfig(ctx context.Context, binding realtimeLoginBinding) (verifiedRealtimeConnection, error) {
	scope := binding.key
	if binding.role == "" || binding.role != realtimeRole(binding.ownerID) {
		return verifiedRealtimeConnection{}, realtime.ErrSourceUnavailable
	}
	writer, err := s.dsgaEndpointRuntime(ctx, scope.Project, binding.writer)
	if err != nil || writer == nil || writer.Payload.BranchID != scope.Branch ||
		writer.Payload.TimelineID != scope.Timeline || writer.Payload.EndpointType != "read_write" {
		return verifiedRealtimeConnection{}, realtime.ErrSourceUnavailable
	}
	if binding.maxWALBytes > 0 && (writer.Plan.SlotWALKeepMiB == 0 || int64(writer.Plan.SlotWALKeepMiB)<<20 > binding.maxWALBytes) {
		return verifiedRealtimeConnection{}, realtime.ErrSlotWALBudget
	}
	_, vm, _, _, err := s.readDSGABoundSelection(ctx, writer)
	if err != nil || vm == nil {
		return verifiedRealtimeConnection{}, realtime.ErrSourceUnavailable
	}
	secret, err := s.kube.request(ctx, http.MethodGet, s.kube.path("secret", binding.credential), nil)
	if err != nil || !owned(secret, scope.Project, binding.writer) || nested(secret, "metadata", "uid") != binding.credentialUID ||
		nested(secret, "metadata", "labels", "neon-control/branch-id") != scope.Branch ||
		nested(secret, "metadata", "labels", binding.ownerLabel) != binding.ownerID || secret["immutable"] != true {
		return verifiedRealtimeConnection{}, realtime.ErrSourceUnavailable
	}
	role, err := secretText(secret, "user")
	if err != nil || role != binding.role {
		return verifiedRealtimeConnection{}, realtime.ErrSourceUnavailable
	}
	password, err := secretText(secret, "password")
	if err != nil || password == "" {
		return verifiedRealtimeConnection{}, realtime.ErrSourceUnavailable
	}
	config, timeout, err := sqlConnectionConfig(s.proxyHost, s.proxyPort, role, password, scope.Database, selector(binding.writer))
	if err != nil {
		return verifiedRealtimeConnection{}, realtime.ErrSourceUnavailable
	}
	return verifiedRealtimeConnection{config: config, timeout: timeout, tenantID: writer.Payload.TenantID}, nil
}

func (s *server) openRealtimeSQL(ctx context.Context, binding realtimeLoginBinding) (*pgx.Conn, error) {
	verified, err := s.realtimeLoginConfig(ctx, binding)
	if err != nil {
		return nil, realtime.ErrSourceUnavailable
	}
	connect, cancelConnect := context.WithTimeout(ctx, verified.timeout)
	conn, err := pgx.ConnectConfig(connect, verified.config)
	cancelConnect()
	if err != nil {
		return nil, realtime.ErrSourceUnavailable
	}
	var correct bool
	err = conn.QueryRow(ctx, `SELECT current_setting('neon.project_id',true)=$1
 AND current_setting('neon.branch_id',true)=$2 AND current_setting('neon.timeline_id',true)=$3
 AND current_database()=$4 AND session_user=$5 AND current_user=$5 AND NOT pg_is_in_recovery()
 AND current_setting('neon.tenant_id',true)=$6`,
		binding.key.Project, binding.key.Branch, binding.key.Timeline, binding.key.Database, binding.role, verified.tenantID).Scan(&correct)
	if err != nil || !correct {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = conn.Close(cleanup)
		return nil, realtime.ErrSourceUnavailable
	}
	return conn, nil
}

type realtimeWorkerSource struct {
	realtimeLoginBinding
	maxFrameBytes int
}

func (s *server) realtimeWantedSources(ctx context.Context, limit int) ([]realtimeWorkerSource, error) {
	rows, err := s.db.Query(ctx, `
SELECT s.id,s.project_id,s.branch_id,s.timeline_id,s.database_name,r.writer_id,
       r.credential_ref,r.credential_uid,r.max_frame_bytes,r.max_wal_bytes
FROM realtime_sources s JOIN realtime_source_runtime r ON r.source_id=s.id
JOIN projects p ON p.id=s.project_id AND p.state='ready' AND p.deleted_at IS NULL
JOIN branches b ON b.id=s.branch_id AND b.timeline_id=s.timeline_id AND b.state='ready' AND b.deleted_at IS NULL
JOIN endpoints e ON e.id=r.writer_id AND e.state='active' AND e.deleted_at IS NULL AND e.endpoint_type='read_write'
WHERE s.retired_at IS NULL AND EXISTS (SELECT 1 FROM realtime_demand d WHERE d.source_id=s.id AND d.expires_at>clock_timestamp())
ORDER BY s.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var desired []realtimeWorkerSource
	for rows.Next() {
		var source realtimeWorkerSource
		if err := rows.Scan(&source.key.ID, &source.key.Project, &source.key.Branch, &source.key.Timeline, &source.key.Database,
			&source.writer, &source.credential, &source.credentialUID, &source.maxFrameBytes, &source.maxWALBytes); err != nil {
			return nil, err
		}
		source.role, source.ownerLabel, source.ownerID = realtimeRole(source.key.ID), "neon-control/realtime-source", source.key.ID
		desired = append(desired, source)
	}
	return desired, rows.Err()
}

func (s *server) runRealtimeSource(ctx context.Context, source realtimeWorkerSource) error {
	store := realtime.Store{DB: s.db}
	return store.RunSource(ctx, source.key, realtime.WorkerLimits{LeaseDuration: 45 * time.Second,
		FeedbackInterval: time.Second, MaxFrameBytes: source.maxFrameBytes, MaxWALBytes: source.maxWALBytes},
		func(call context.Context, _ realtime.Lease) (*pgx.Conn, error) {
			conn, err := s.openRealtimeSQL(call, source.realtimeLoginBinding)
			if err != nil {
				return nil, err
			}
			var retained int64
			err = conn.QueryRow(call, "SELECT setting::bigint*1048576 FROM pg_settings WHERE name='max_slot_wal_keep_size' AND unit='MB'").Scan(&retained)
			if err != nil || retained <= 0 || retained > source.maxWALBytes {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = conn.Close(cleanup)
				return nil, realtime.ErrSlotWALBudget
			}
			return conn, nil
		},
		func(call context.Context, _ realtime.Lease) (*pgconn.PgConn, error) {
			verified, err := s.realtimeLoginConfig(call, source.realtimeLoginBinding)
			if err != nil {
				return nil, err
			}
			verified.config.RuntimeParams["replication"] = "database"
			start, cancel := context.WithTimeout(call, verified.timeout)
			defer cancel()
			return pgconn.ConnectConfig(start, &verified.config.Config)
		})
}

func realtimeWorkerLimit() (int, error) {
	if os.Getenv("NEON_REALTIME_ENABLED") != "true" {
		return 0, nil
	}
	limit, err := strconv.Atoi(os.Getenv("NEON_REALTIME_SOURCE_LIMIT"))
	if err != nil || limit <= 0 {
		return 0, errors.New("Realtime requires an explicit source worker budget")
	}
	return limit, nil
}

func (s *server) runRealtimeSources(ctx context.Context, limit int) {
	type runningSource struct {
		cancel context.CancelFunc
		done   chan struct{}
	}
	active := make(map[string]runningSource)
	retryAfter := make(map[string]time.Time)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer func() {
		for _, source := range active {
			source.cancel()
		}
		for _, source := range active {
			<-source.done
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		call, cancel := context.WithTimeout(ctx, time.Second)
		desired, err := s.realtimeWantedSources(call, limit)
		if err == nil {
			_, _ = s.db.Exec(call, "DELETE FROM realtime_demand WHERE expires_at<clock_timestamp()")
		}
		cancel()
		wanted := make(map[string]struct{})
		if err == nil {
			for _, source := range desired {
				wanted[source.key.ID] = struct{}{}
			}
		}
		for id, source := range active {
			if _, ok := wanted[id]; !ok {
				source.cancel()
			}
			select {
			case <-source.done:
				delete(active, id)
				if _, ok := wanted[id]; ok {
					retryAfter[id] = time.Now().Add(5 * time.Second)
				}
			default:
			}
		}
		for id := range retryAfter {
			if _, wanted := wanted[id]; !wanted {
				delete(retryAfter, id)
			}
		}
		for _, source := range desired {
			if _, already := active[source.key.ID]; already || len(active) >= limit || time.Now().Before(retryAfter[source.key.ID]) {
				continue
			}
			run, stop := context.WithCancel(ctx)
			done := make(chan struct{})
			active[source.key.ID] = runningSource{cancel: stop, done: done}
			go func() {
				defer close(done)
				defer stop()
				if err := s.runRealtimeSource(run, source); err != nil && run.Err() == nil {
					outcome := "unavailable"
					if errors.Is(err, realtime.ErrSlotWALBudget) {
						outcome = "retained_wal_budget"
					} else if errors.Is(err, realtime.ErrSlotAhead) {
						outcome = "slot_ahead"
					} else if errors.Is(err, realtime.ErrPublicationChanged) {
						outcome = "publication_changed"
					}
					s.logger.Warn("Realtime source stopped", "source", source.key.ID, "outcome", outcome)
				}
			}()
		}
	}
}

func (s *server) realtimeDemand(ctx context.Context, key realtime.SourceKey, limit int) (context.Context, func(), error) {
	id := newID("rtd_")
	exec, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := pgx.BeginFunc(exec, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(exec, "SELECT pg_advisory_xact_lock(hashtextextended('neon-realtime-demand',0))"); err != nil {
			return err
		}
		var current bool
		var used int
		if err := tx.QueryRow(exec, `SELECT COALESCE(bool_or(source_id=$1),false),count(DISTINCT source_id)
FROM realtime_demand WHERE expires_at>clock_timestamp()`, key.ID).Scan(&current, &used); err != nil {
			return err
		}
		if limit <= 0 || (!current && used >= limit) {
			return realtime.ErrSubscriptionCapacity
		}
		tag, err := tx.Exec(exec, `INSERT INTO realtime_demand(source_id,id,expires_at)
SELECT s.id,$2,clock_timestamp()+interval '45 seconds' FROM realtime_sources s
WHERE s.id=$1 AND s.project_id=$3 AND s.branch_id=$4 AND s.timeline_id=$5 AND s.database_name=$6 AND s.retired_at IS NULL
AND EXISTS (SELECT 1 FROM realtime_source_runtime r WHERE r.source_id=s.id)`,
			key.ID, id, key.Project, key.Branch, key.Timeline, key.Database)
		if err != nil || tag.RowsAffected() != 1 {
			return realtime.ErrSourceUnavailable
		}
		return nil
	})
	cancel()
	if err != nil {
		return nil, nil, err
	}
	stream, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stream.Done():
				return
			case <-ticker.C:
				refresh, cancel := context.WithTimeout(stream, 5*time.Second)
				tag, err := s.db.Exec(refresh, `UPDATE realtime_demand d SET expires_at=clock_timestamp()+interval '45 seconds'
FROM realtime_sources s WHERE d.id=$1 AND d.source_id=$2 AND d.source_id=s.id AND s.retired_at IS NULL`, id, key.ID)
				cancel()
				if err != nil || tag.RowsAffected() != 1 {
					stop()
					return
				}
			}
		}
	}()
	return stream, func() {
		stop()
		<-done
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = s.db.Exec(cleanup, "DELETE FROM realtime_demand WHERE id=$1 AND source_id=$2", id, key.ID)
	}, nil
}
