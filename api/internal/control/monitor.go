package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const statisticsSQL = `SELECT
 (SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND backend_type='client backend'
  AND usename <> 'control_probe' AND client_addr IS NOT NULL
  AND NOT (client_addr <<= '127.0.0.0/8'::inet) AND NOT (client_addr <<= '::1/128'::inet)),
 (SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND backend_type='client backend'
  AND state='active' AND usename <> 'control_probe' AND client_addr IS NOT NULL
  AND NOT (client_addr <<= '127.0.0.0/8'::inet) AND NOT (client_addr <<= '::1/128'::inet)),
 (SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND backend_type='client backend'
  AND state='idle' AND usename <> 'control_probe' AND client_addr IS NOT NULL
  AND NOT (client_addr <<= '127.0.0.0/8'::inet) AND NOT (client_addr <<= '::1/128'::inet)),
 pg_database_size(current_database()),deadlocks,tup_inserted,tup_updated,tup_deleted
 FROM pg_stat_database WHERE datname=current_database()`

// On a DSGA Compute, private system connections use Unix sockets and the
// platform probe has a reserved login. Count ordinary sessions on all DBs.
// Unknown activity blocks scale-to-zero when tracking is disabled.
const dsgaStatisticsSQL = `SELECT
 (SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE backend_type='client backend'
  AND usename <> 'control_probe' AND (client_addr IS NOT NULL OR usename <> 'cloud_admin')),
 (SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE backend_type='client backend'
  AND state='active' AND usename <> 'control_probe' AND (client_addr IS NOT NULL OR usename <> 'cloud_admin')),
 (SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE backend_type='client backend'
  AND state='idle' AND usename <> 'control_probe' AND (client_addr IS NOT NULL OR usename <> 'cloud_admin')),
 pg_catalog.pg_database_size(pg_catalog.current_database()),deadlocks,tup_inserted,tup_updated,tup_deleted,
 pg_catalog.pg_postmaster_start_time(),current_setting('neon.tenant_id'),current_setting('neon.timeline_id'),
 (SELECT pg_catalog.sum(sessions)::bigint FROM pg_catalog.pg_stat_database)
 FROM pg_catalog.pg_stat_database WHERE datname=pg_catalog.current_database()
 AND current_setting('track_activities')='on'`

func (s *server) runMonitor(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	s.collect(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.collect(ctx)
		}
	}
}

func quantityCPU(value string) float64 {
	multiplier := 1000.0
	if strings.HasSuffix(value, "n") {
		value = strings.TrimSuffix(value, "n")
		multiplier = 0.000001
	} else if strings.HasSuffix(value, "u") {
		value = strings.TrimSuffix(value, "u")
		multiplier = 0.001
	} else if strings.HasSuffix(value, "m") {
		value = strings.TrimSuffix(value, "m")
		multiplier = 1
	}
	n, _ := strconv.ParseFloat(value, 64)
	return n * multiplier
}
func quantityMemory(value string) float64 {
	for _, unit := range []struct {
		suffix string
		factor float64
	}{{"Ki", 1.0 / 1024}, {"Mi", 1}, {"Gi", 1024}, {"K", 1000.0 / 1048576}, {"M", 1000000.0 / 1048576}, {"G", 1000000000.0 / 1048576}} {
		if strings.HasSuffix(value, unit.suffix) {
			n, _ := strconv.ParseFloat(strings.TrimSuffix(value, unit.suffix), 64)
			return n * unit.factor
		}
	}
	n, _ := strconv.ParseFloat(value, 64)
	return n / 1048576
}

func (s *server) collect(ctx context.Context) {
	items, err := s.many(ctx, `SELECT e.id,e.project_id,e.workload_kind,e.workload_name,e.selector,e.database_name,e.state,p.source,
	 `+dsgaEndpointLifecycleExists+` AS dsga
        FROM endpoints e JOIN projects p ON p.id=e.project_id WHERE e.deleted_at IS NULL`)
	if err != nil {
		s.logger.Error("monitor endpoint list", "error", err)
		return
	}
	for _, endpoint := range items {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := s.sample(ctx, endpoint); err != nil {
			s.logger.Error("monitor sample", "endpoint_id", endpoint["id"], "error", err)
		}
	}
	_, _ = s.db.Exec(ctx, "DELETE FROM metric_samples WHERE sampled_at < now()-interval '7 days'")
}

func (s *server) sample(parent context.Context, endpoint record) error {
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	if endpoint["source"] == "managed" && endpoint["workload_kind"] == "neonvm" {
		// Runtime observation and subsequent observational SQL share a lock
		// with suspend. A stale observation must never wake a deleted VM.
		release, err := s.acquireProbeGate(ctx, stringVal(endpoint["id"]), true)
		if err != nil {
			return err
		}
		defer release()
	}
	runtime := s.kube.runtime(ctx, stringVal(endpoint["workload_kind"]), stringVal(endpoint["workload_name"]))
	state := stringVal(runtime["observed_state"])
	errorsBySource := map[string]string{}
	values := make([]any, 12)
	var dsgaVMUID *string
	var pgStartedAt *time.Time
	var pgSessions *int64
	managedDSGA := endpoint["dsga"] == true
	if managedDSGA && stringVal(runtime["workload_uid"]) != "" {
		uid := stringVal(runtime["workload_uid"])
		dsgaVMUID = &uid
	}
	values[0] = runtime["cpu_milli"]
	values[2] = runtime["memory_mib"]
	pod := stringVal(runtime["pod_name"])
	if pod != "" && state == "active" {
		item, err := s.kube.request(ctx, "GET", "/apis/metrics.k8s.io/v1beta1/namespaces/"+url.PathEscape(s.kube.namespace)+"/pods/"+url.PathEscape(pod), nil)
		if err != nil {
			errorsBySource["pod_usage"] = "metrics_unavailable"
		} else if containers, ok := item["containers"].([]any); ok {
			cpu, mem := 0.0, 0.0
			for _, entry := range containers {
				if c, ok := entry.(map[string]any); ok {
					cpu += quantityCPU(stringVal(nested(c, "usage", "cpu")))
					mem += quantityMemory(stringVal(nested(c, "usage", "memory")))
				}
			}
			values[1], values[3] = cpu, mem
		}
	}
	if managedDSGA && state == "active" {
		result, boot, sessions, err := s.sampleDSGAStats(ctx, endpoint, dsgaVMUID)
		if err != nil {
			errorsBySource["postgres"] = "dsga_observation_unavailable"
		} else {
			copy(values[4:], result)
			pgStartedAt = &boot
			pgSessions = &sessions
		}
	} else if endpoint["source"] == "managed" && endpoint["workload_kind"] == "neonvm" && state == "active" {
		secret, err := s.kube.request(ctx, "GET", s.kube.path("secret", stringVal(endpoint["workload_name"])+"-config"), nil)
		if err != nil {
			errorsBySource["postgres"] = "probe_secret_unavailable"
		} else {
			raw := stringVal(nested(secret, "data", "probePassword"))
			decoded, err := base64.StdEncoding.DecodeString(raw)
			if err != nil {
				errorsBySource["postgres"] = "probe_secret_invalid"
			} else {
				result, err := runSQL(ctx, s.proxyHost, s.proxyPort, "control_probe", string(decoded),
					stringVal(endpoint["database_name"]), stringVal(endpoint["selector"]), statisticsSQL)
				if err != nil || len(result.Rows) != 1 || len(result.Rows[0]) != 8 {
					errorsBySource["postgres"] = "query_failed"
				} else {
					for i, value := range result.Rows[0] {
						values[4+i] = value
					}
				}
			}
		}
	} else if endpoint["source"] == "managed" {
		errorsBySource["postgres"] = "compute_not_running"
	} else {
		errorsBySource["postgres"] = "probe_unavailable"
	}
	if runtime["error"] != nil {
		errorsBySource["kubernetes"] = "runtime_unavailable"
	}
	errs, _ := json.Marshal(errorsBySource)
	_, err := s.db.Exec(ctx, `INSERT INTO metric_samples(endpoint_id,sampled_at,observed_state,
        cpu_allocated_milli,cpu_used_milli,memory_allocated_mib,memory_used_mib,
        connections,active_connections,idle_connections,database_size_bytes,deadlocks_total,
        rows_inserted_total,rows_updated_total,rows_deleted_total,errors,dsga_vm_uid,dsga_pg_started_at,dsga_pg_sessions)
        VALUES($1,now(),$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		endpoint["id"], state, values[0], values[1], values[2], values[3], values[4], values[5], values[6], values[7], values[8], values[9], values[10], values[11], string(errs), dsgaVMUID, pgStartedAt, pgSessions)
	if err != nil {
		return fmt.Errorf("metric insert: %w", err)
	}
	return nil
}

func (s *server) sampleDSGAStats(ctx context.Context, endpoint record, uid *string) ([]any, time.Time, int64, error) {
	e, err := s.dsgaEndpointRuntime(ctx, stringVal(endpoint["project_id"]), stringVal(endpoint["id"]))
	if err != nil {
		return nil, time.Time{}, 0, err
	}
	if e == nil || uid == nil || e.VMUID != *uid || stringVal(endpoint["selector"]) != selector(e.Payload.EndpointID) {
		return nil, time.Time{}, 0, errDSGACreationIdentity
	}
	clients, vm, _, _, err := s.readDSGABoundSelection(ctx, e)
	if err != nil {
		return nil, time.Time{}, 0, err
	}
	name := vm.Name + "-credentials"
	secret, err := clients.CoreV1().Secrets(vm.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, time.Time{}, 0, err
	}
	password, err := readDSGABoundProbePassword(secret, e)
	if err != nil {
		return nil, time.Time{}, 0, err
	}
	result, err := runSQL(ctx, s.proxyHost, s.proxyPort, "control_probe", password,
		dsgaApplicationDatabase, selector(e.Payload.EndpointID), dsgaStatisticsSQL)
	if err != nil || len(result.Rows) != 1 || len(result.Rows[0]) != 12 {
		return nil, time.Time{}, 0, errDSGAInitializationPending
	}
	row := result.Rows[0]
	boot, ok := row[8].(time.Time)
	sessions, counted := row[11].(int64)
	if !ok || !counted || sessions < 0 || boot.Before(vm.CreationTimestamp.Time) || boot.After(time.Now()) ||
		row[9] != e.Payload.TenantID || row[10] != e.Payload.TimelineID {
		return nil, time.Time{}, 0, errDSGACreationIdentity
	}
	_, latest, _, _, err := s.readDSGABoundSelection(ctx, e)
	if err != nil || vm.UID != latest.UID || vm.Status.PodName != latest.Status.PodName {
		return nil, time.Time{}, 0, errDSGACreationIdentity
	}
	return row[:8], boot, sessions, nil
}
