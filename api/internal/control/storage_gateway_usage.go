package control

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type storageUsageKey struct{}

// Values come only from server I/O and canonical metadata. Caller byte headers,
// request IDs, object names and credentials are never used as receipts.
type storageUsage struct {
	s                             *server
	id, project, branch, protocol string
	started                       time.Time
	ingress, egress               int64
	status                        int
	failed                        bool
}

type storageUsageBody struct {
	io.ReadCloser
	usage *storageUsage
}

func (b *storageUsageBody) Read(p []byte) (int, error) {
	// Also bound signed-chunk wire overhead, not only decoded payload bytes.
	remaining := storageTransferReservation() - b.usage.ingress - b.usage.egress
	if remaining <= 0 {
		b.usage.failed = true
		return 0, errStoragePayloadSize
	}
	if int64(len(p)) > remaining {
		p = p[:int(remaining)]
	}
	n, err := b.ReadCloser.Read(p)
	b.usage.ingress += int64(n)
	if err != nil && err != io.EOF {
		b.usage.failed = true
	}
	return n, err
}

type storageUsageWriter struct {
	http.ResponseWriter
	usage *storageUsage
	head  bool
}

func (w *storageUsageWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *storageUsageWriter) WriteHeader(status int) {
	if status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.usage.status != 0 {
		return
	}
	if w.usage.id != "" {
		w.Header().Set("X-Neon-Storage-Usage-Id", w.usage.id)
	}
	w.usage.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *storageUsageWriter) Write(p []byte) (int, error) {
	if w.usage.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	allowed := len(p)
	if w.usage.id != "" && !w.head {
		remaining := storageTransferReservation() - w.usage.ingress - w.usage.egress
		if remaining < int64(allowed) {
			allowed = int(max(remaining, 0))
		}
	}
	n, err := w.ResponseWriter.Write(p[:allowed])
	if err == nil && allowed != len(p) {
		err = io.ErrShortWrite
	}
	if !w.head {
		w.usage.egress += int64(n)
	}
	if err != nil || n != len(p) {
		w.usage.failed = true
	}
	return n, err
}
func (w *storageUsageWriter) FlushError() error {
	if w.usage.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err != nil {
		w.usage.failed = true
	}
	return err
}
func (w *storageUsageWriter) Flush() { _ = w.FlushError() }

func (s *server) storageUsageHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Value(storageUsageKey{}).(*storageUsage); ok {
			next.ServeHTTP(w, r)
			return
		}
		protocol := "s3"
		if strings.HasPrefix(r.URL.Path, "/storage/v1/") || strings.HasPrefix(r.URL.Path, "/api/") {
			protocol = "rest"
		}
		u := &storageUsage{s: s, protocol: protocol, started: time.Now()}
		r = r.WithContext(context.WithValue(r.Context(), storageUsageKey{}, u))
		if r.Body != nil {
			r.Body = &storageUsageBody{ReadCloser: r.Body, usage: u}
		}
		writer := &storageUsageWriter{ResponseWriter: w, usage: u, head: r.Method == http.MethodHead}
		returned := false
		defer func() {
			if !returned || r.Context().Err() != nil {
				u.failed = true
			}
			if u.id != "" {
				u.finish()
			}
		}()
		next.ServeHTTP(writer, r)
		returned = true
	})
}

var errStorageTenantCapacity = errors.New("storage tenant admission exhausted")

// Durable admission commits before reserving lifecycle connections or changing
// branch SQL/blobs. Anonymous authorization still runs in the native handlers;
// its SQL checks consume tenant work and retain their actual denial status.
func (s *server) admitStorageUsage(r *http.Request, expected, branch string) error {
	u, _ := r.Context().Value(storageUsageKey{}).(*storageUsage)
	if u == nil {
		return nil
	} // Direct internal calls/tests are not HTTP receipts.
	if u.id != "" {
		if branch != u.branch || expected != "" && expected != u.project {
			return errors.New("storage usage identity changed")
		}
		return nil
	}
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var project string
	err = tx.QueryRow(r.Context(), `SELECT b.project_id FROM branches b JOIN projects p ON p.id=b.project_id JOIN object_storage_instances a ON a.branch_id=b.id WHERE b.id=$1 AND ($2='' OR b.project_id=$2) AND b.state='ready' AND b.deleted_at IS NULL AND p.state='ready' AND p.deleted_at IS NULL AND a.state='active'`, branch, expected).Scan(&project)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,952781))`, project); err != nil {
		return err
	}
	var concurrent int
	var dailyRequests, dailyBytes int64
	err = tx.QueryRow(r.Context(), `SELECT concurrent_requests,daily_requests,daily_transfer_bytes FROM storage_gateway_limits WHERE project_id=$1`, project).Scan(&concurrent, &dailyRequests, &dailyBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		concurrent = 4
	} else if err != nil {
		return err
	}
	var active int
	if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM storage_gateway_requests WHERE project_id=$1 AND state='admitted' AND lease_until>clock_timestamp()`, project).Scan(&active); err != nil {
		return err
	}
	if active >= concurrent {
		return errStorageTenantCapacity
	}
	var day time.Time
	if err = tx.QueryRow(r.Context(), `SELECT (clock_timestamp() AT TIME ZONE 'UTC')::date`).Scan(&day); err != nil {
		return err
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO storage_gateway_days(project_id,period) VALUES($1,$2) ON CONFLICT DO NOTHING`, project, day); err != nil {
		return err
	}
	var requests, observed, reserved int64
	if err = tx.QueryRow(r.Context(), `SELECT requests,observed_bytes,reserved_bytes FROM storage_gateway_days WHERE project_id=$1 AND period=$2 FOR UPDATE`, project, day).Scan(&requests, &observed, &reserved); err != nil {
		return err
	}
	budget := storageTransferReservation()
	if dailyRequests > 0 && requests >= dailyRequests || dailyBytes > 0 && observed+reserved > dailyBytes-budget {
		return errStorageTenantCapacity
	}
	id := newID("sgr_")
	_, err = tx.Exec(r.Context(), `INSERT INTO storage_gateway_requests(id,project_id,branch_id,period,method,protocol,lease_until,reserved_bytes) VALUES($1,$2,$3,$4,$5,$6,clock_timestamp()+$7*interval '1 millisecond',$8)`, id, project, branch, day, r.Method, u.protocol, (currentStorageCapacity().TransferTimeout + 30*time.Second).Milliseconds(), budget)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(r.Context(), `UPDATE storage_gateway_days SET requests=requests+1,reserved_bytes=reserved_bytes+$3 WHERE project_id=$1 AND period=$2`, project, day, budget); err != nil {
		return err
	}
	if err = tx.Commit(r.Context()); err != nil {
		return err
	}
	u.id, u.project, u.branch = id, project, branch
	return nil
}

func storageTransferReservation() int64 { return 2*storageObjectAdmissionLimit() + (8 << 20) }

func (u *storageUsage) finish() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	state := "completed"
	if u.failed {
		state = "interrupted"
	}
	status := u.status
	if status == 0 {
		status = http.StatusOK
		if u.failed {
			status = http.StatusInternalServerError
		}
	}
	// Receipt and quota update atomically, exactly once. A lost receipt keeps its
	// reservation; lease expiry never fabricates zero usage or a successful result.
	_, err := u.s.db.Exec(ctx, `WITH done AS (
 UPDATE storage_gateway_requests SET state=$2,finished_at=clock_timestamp(),http_status=$3,ingress_bytes=$4,egress_bytes=$5,duration_ms=$6
 WHERE id=$1 AND state='admitted' RETURNING project_id,period,reserved_bytes)
 UPDATE storage_gateway_days d SET observed_bytes=d.observed_bytes+$4+$5,reserved_bytes=d.reserved_bytes-done.reserved_bytes
 FROM done WHERE d.project_id=done.project_id AND d.period=done.period`, u.id, state, status, u.ingress, u.egress, time.Since(u.started).Milliseconds())
	if err != nil && u.s.logger != nil {
		u.s.logger.Error("storage usage finalization unavailable", "usage_id", u.id)
	}
}
