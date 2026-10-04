package control

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type processRole string

const (
	roleAll    processRole = "all"
	roleAPI    processRole = "api"
	roleWorker processRole = "worker"
	// One active controller group per metadata database. This session lock
	// serializes cooperative workers; it is NOT an external Proxy/VM fence.
	controllerLockID int64 = 794210027
)

var errControllerLeadershipLost = errors.New("controller leadership lost")

func parseProcessRole(value string) (processRole, error) {
	switch role := processRole(value); role {
	case roleAll, roleAPI, roleWorker:
		return role, nil
	default:
		return "", errors.New("NEON_CONTROL_PROCESS_ROLE must be all, api or worker")
	}
}

type controllerState struct{ leading atomic.Bool }

type controllerLease struct {
	connection *pgxpool.Conn
	owner      string
	epoch      int64
}

// Retain a dedicated PostgreSQL session for the advisory lock. Returning this
// connection to the pool while locked would transfer ownership accidentally.
func acquireControllerLease(ctx context.Context, db *pgxpool.Pool, role processRole) (*controllerLease, error) {
	if role != roleAll && role != roleWorker {
		return nil, errors.New("API process cannot acquire controller leadership")
	}
	connection, err := db.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	var locked bool
	if err = connection.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", controllerLockID).Scan(&locked); err != nil {
		connection.Conn().Close(ctx)
		connection.Release()
		return nil, err
	}
	if !locked {
		connection.Release()
		return nil, nil
	}
	lease := &controllerLease{connection: connection, owner: newID("controller_")}
	err = connection.QueryRow(ctx, `INSERT INTO control_runtime_leases(name,owner_id,epoch,process_role,lease_expires_at)
		VALUES('controllers',$1,1,$2,clock_timestamp()+interval '10 seconds')
		ON CONFLICT(name) DO UPDATE SET owner_id=EXCLUDED.owner_id,epoch=control_runtime_leases.epoch+1,
		process_role=EXCLUDED.process_role,lease_expires_at=EXCLUDED.lease_expires_at,updated_at=clock_timestamp()
		RETURNING epoch`, lease.owner, string(role)).Scan(&lease.epoch)
	if err != nil {
		lease.close()
		return nil, err
	}
	return lease, nil
}

func (lease *controllerLease) heartbeat(ctx context.Context) error {
	if lease.connection == nil {
		return errControllerLeadershipLost
	}
	tag, err := lease.connection.Exec(ctx, `UPDATE control_runtime_leases
		SET lease_expires_at=clock_timestamp()+interval '10 seconds',updated_at=clock_timestamp()
		WHERE name='controllers' AND owner_id=$1 AND epoch=$2`, lease.owner, lease.epoch)
	if err != nil || tag.RowsAffected() != 1 {
		return errControllerLeadershipLost
	}
	return nil
}

func (lease *controllerLease) close() {
	if lease.connection == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Expire only this generation, never a successor's record. Closing the
	// physical session also releases its advisory lock after any SQL failure.
	lease.connection.Exec(ctx, `UPDATE control_runtime_leases SET lease_expires_at=clock_timestamp(),
		updated_at=clock_timestamp() WHERE name='controllers' AND owner_id=$1 AND epoch=$2`, lease.owner, lease.epoch)
	lease.connection.Conn().Close(ctx)
	lease.connection.Release()
	lease.connection = nil
}

func (s *server) runLeaderControllers(ctx context.Context, role processRole, state *controllerState) error {
	var lease *controllerLease
	for lease == nil {
		if ctx.Err() != nil {
			return nil
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var err error
		lease, err = acquireControllerLease(attemptCtx, s.db, role)
		cancel()
		if err != nil {
			// Do not include DSNs, PostgreSQL secrets or query parameters in logs.
			return errors.New("controller leadership acquisition unavailable")
		}
		if lease == nil {
			if role == roleAll {
				return errors.New("another controller is active; combined mode cannot overlap")
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
		}
	}
	defer lease.close()
	controllerCtx, cancelControllers := context.WithCancel(ctx)
	defer cancelControllers()
	var group sync.WaitGroup
	start := func(run func(context.Context)) {
		group.Add(1)
		go func() { defer group.Done(); run(controllerCtx) }()
	}
	start(s.runWorker)
	start(s.runMonitor)
	start(s.cleanSessions)
	if os.Getenv("NEON_V2_SCALE_ZERO_ENABLED") == "true" {
		start(s.runIdleController)
	}
	state.leading.Store(true)
	s.logger.Info("controller leadership acquired", "epoch", lease.epoch, "role", role)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var result error
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
			beatCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := lease.heartbeat(beatCtx)
			cancel()
			if err != nil {
				result = errControllerLeadershipLost
				break loop
			}
		}
	}
	state.leading.Store(false)
	cancelControllers()
	drained := make(chan struct{})
	go func() { group.Wait(); close(drained) }()
	select {
	case <-drained:
		return result
	case <-time.After(12 * time.Second):
		return errors.New("controller drain deadline exceeded")
	}
}

// Worker health is private and exposes neither control routes nor credentials.
func (s *server) workerHealthHandler(state *controllerState) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 200, record{"status": "ok", "role": "worker"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !state.leading.Load() {
			jsonResponse(w, 503, record{"status": "standby", "role": "worker"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := s.db.Ping(ctx); err != nil {
			jsonResponse(w, 503, record{"status": "unavailable", "role": "worker"})
			return
		}
		jsonResponse(w, 200, record{"status": "ready", "role": "worker"})
	})
	return mux
}

func (s *server) runtimeStatus(ctx context.Context) record {
	role := s.processRole
	if role == "" {
		role = roleAll
	}
	status := record{"process_role": role, "separated": role == roleAPI, "controller_status": "unavailable"}
	if s.db == nil {
		return status
	}
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var epoch int64
	var live bool
	var updated time.Time
	err := s.db.QueryRow(readCtx, `SELECT epoch,lease_expires_at>clock_timestamp(),updated_at
		FROM control_runtime_leases WHERE name='controllers'`).Scan(&epoch, &live, &updated)
	if err != nil {
		return status
	}
	status["controller_status"] = "stale"
	if live {
		status["controller_status"] = "active"
	}
	status["epoch"], status["last_heartbeat_at"] = epoch, updated.UTC()
	return status
}
