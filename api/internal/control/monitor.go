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
	items, err := s.many(ctx, `SELECT e.id,e.workload_kind,e.workload_name,e.selector,e.database_name,e.state,p.source
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
	if endpoint["source"] == "managed" && endpoint["workload_kind"] == "neonvm" && state == "active" {
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
        rows_inserted_total,rows_updated_total,rows_deleted_total,errors)
        VALUES($1,now(),$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		endpoint["id"], state, values[0], values[1], values[2], values[3], values[4], values[5], values[6], values[7], values[8], values[9], values[10], values[11], string(errs))
	if err != nil {
		return fmt.Errorf("metric insert: %w", err)
	}
	return nil
}
