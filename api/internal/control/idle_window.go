package control

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type idleSample struct {
	At            time.Time
	State         string
	Connections   *int
	PostgresError bool
}

type dsgaIdleCandidate struct {
	vmUID  string
	window int
}

func dsgaIdleEligible(ctx context.Context, tx pgx.Tx, endpoint, vmUID string, since time.Time, seconds int) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT sampled_at,observed_state,connections,
 COALESCE(errors ? 'postgres',false),dsga_pg_started_at,dsga_pg_sessions
 FROM metric_samples WHERE endpoint_id=$1 AND dsga_vm_uid=$2 AND sampled_at >= $3
 AND sampled_at > clock_timestamp()-make_interval(secs=>$4::int+90)
 ORDER BY sampled_at`, endpoint, vmUID, since, seconds)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var samples []idleSample
	var boot time.Time
	var total int64
	for rows.Next() {
		var sample idleSample
		var started *time.Time
		var sessions *int64
		if err := rows.Scan(&sample.At, &sample.State, &sample.Connections, &sample.PostgresError, &started, &sessions); err != nil {
			return false, err
		}
		valid := started != nil && !started.IsZero() && !started.After(sample.At) && sessions != nil && *sessions >= 0
		// Each sample opens exactly one probe session. Any additional session,
		// missing probe counter or activity counter reset restarts the window.
		if !valid || !started.Equal(boot) || *sessions <= total || *sessions-total != 1 ||
			sample.State != "active" || sample.Connections == nil || *sample.Connections != 0 || sample.PostgresError {
			samples = nil
		}
		if valid {
			boot, total = *started, *sessions
			samples = append(samples, sample)
		} else {
			boot, total = time.Time{}, 0
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	rows.Close()
	var now time.Time
	if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return false, err
	}
	return idleWindowEligible(samples, now, since, time.Duration(seconds)*time.Second), nil
}

const idleSamplesSQL = `SELECT sampled_at,observed_state,connections,
 COALESCE(errors ? 'postgres',false) FROM metric_samples
 WHERE endpoint_id=$1 AND sampled_at >= $2
 AND sampled_at > now()-make_interval(secs=>$3::int+90)
 ORDER BY sampled_at`

func (s *server) readIdleSamples(ctx context.Context, endpoint string, born time.Time, seconds int) ([]idleSample, error) {
	rows, err := s.db.Query(ctx, idleSamplesSQL, endpoint, born, seconds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	samples := []idleSample{}
	for rows.Next() {
		var sample idleSample
		if err := rows.Scan(&sample.At, &sample.State, &sample.Connections, &sample.PostgresError); err != nil {
			return nil, err
		}
		samples = append(samples, sample)
	}
	return samples, rows.Err()
}

// Only a continuous sequence of successful, active, zero-client observations
// of this VM generation can qualify. Suspended/unknown/error/NULL rows reset
// the window, rather than contributing to count/min while max ignores NULL.
// Missing intervals reset it too. No scheduling allowance shortens the policy.
// This is observational idle evidence, not external Proxy admission fencing:
// short connections between samples still require the future activity ledger.
func idleWindowEligible(samples []idleSample, now, born time.Time, window time.Duration) bool {
	if born.IsZero() || window < time.Minute || born.After(now) || now.Sub(born) < window {
		return false
	}
	var first, last time.Time
	count := 0
	var previous time.Time
	for _, sample := range samples {
		if sample.At.After(now) || (!previous.IsZero() && !sample.At.After(previous)) {
			return false
		}
		previous = sample.At
		if sample.At.Before(born) {
			continue
		}
		if sample.State != "active" || sample.Connections == nil || *sample.Connections != 0 || sample.PostgresError {
			first, last, count = time.Time{}, time.Time{}, 0
			continue
		}
		if last.IsZero() || sample.At.Sub(last) > 45*time.Second {
			first, count = sample.At, 0
		}
		last = sample.At
		count++
	}
	return count >= 2 && !first.IsZero() && last.Sub(first) >= window && now.Sub(last) <= 45*time.Second
}
