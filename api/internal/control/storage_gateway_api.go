package control

import (
	"net/http"
	"strings"
)

// The API process keeps Console authorization and management REST routes. Its
// Storage I/O shares the independent gateway's ledger and tenant quotas.
// This wrapper touches only Storage routes so WebSocket interfaces stay intact.
func (s *server) withStorageGatewayRoutes(mux *http.ServeMux) http.Handler {
	mux.Handle("GET /api/v1/projects/{project}/branches/{branch}/storage/usage", s.auth(http.HandlerFunc(s.storageGatewayUsageRead), false))
	mux.Handle("GET /api/v1/projects/{project}/storage-gateway", s.auth(http.HandlerFunc(s.storageGatewayLimitsRead), false))
	metered := s.storageUsageHandler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/storage/v1/") || strings.HasPrefix(r.URL.Path, "/api/v1/projects/") && strings.Contains(r.URL.Path, "/storage/") {
			metered.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *server) storageGatewayUsageRead(w http.ResponseWriter, r *http.Request) {
	project, branch := r.PathValue("project"), r.PathValue("branch")
	var exists bool
	if err := s.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM branches WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL)`, branch, project).Scan(&exists); err != nil || !exists {
		fail(w, r, 404, "not_found", "Branch not found")
		return
	}
	items, err := s.many(r.Context(), `SELECT id,method,protocol,CASE WHEN state='admitted' AND lease_until<=clock_timestamp() THEN 'unknown' ELSE state END AS state,started_at,finished_at,http_status,ingress_bytes,egress_bytes,duration_ms,reserved_bytes FROM storage_gateway_requests WHERE project_id=$1 AND branch_id=$2 ORDER BY started_at DESC,id DESC LIMIT 50`, project, branch)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Storage usage unavailable")
		return
	}
	summary, err := s.one(r.Context(), `SELECT count(*) AS requests,count(*) FILTER(WHERE state='completed' AND http_status<400) AS successful_responses,count(*) FILTER(WHERE state='completed' AND http_status>=400) AS error_responses,count(*) FILTER(WHERE state='interrupted') AS interrupted,count(*) FILTER(WHERE state='admitted') AS unfinished_or_unknown,COALESCE(sum(ingress_bytes),0) AS observed_ingress_bytes,COALESCE(sum(egress_bytes),0) AS observed_egress_bytes FROM storage_gateway_requests WHERE project_id=$1 AND branch_id=$2 AND period=(clock_timestamp() AT TIME ZONE 'UTC')::date`, project, branch)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Storage usage unavailable")
		return
	}
	jsonResponse(w, 200, record{"items": items, "summary": summary, "period": "UTC day", "source": "server_observed_http_body_bytes", "billing_enabled": false, "physical_capacity_measured": false, "unknown_usage_is_zero": false})
}

func (s *server) storageGatewayLimitsRead(w http.ResponseWriter, r *http.Request) {
	item, err := s.one(r.Context(), `SELECT p.id AS project_id,COALESCE(l.generation,0) AS generation,COALESCE(l.concurrent_requests,4) AS concurrent_requests,COALESCE(l.daily_requests,0) AS daily_requests,COALESCE(l.daily_transfer_bytes,0) AS daily_transfer_bytes,COALESCE(d.requests,0) AS admitted_requests,COALESCE(d.observed_bytes,0) AS observed_bytes,COALESCE(d.reserved_bytes,0) AS reserved_bytes FROM projects p LEFT JOIN storage_gateway_limits l ON l.project_id=p.id LEFT JOIN storage_gateway_days d ON d.project_id=p.id AND d.period=(clock_timestamp() AT TIME ZONE 'UTC')::date WHERE p.id=$1 AND p.deleted_at IS NULL`, r.PathValue("project"))
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Storage admission unavailable")
		return
	}
	item["period"] = "UTC day"
	item["zero_limit_means"] = "unlimited"
	item["per_request_transfer_reservation_bytes"] = storageTransferReservation()
	item["limits_managed_by"] = "operator"
	item["object_bytes"] = storageObjectAdmissionLimit()
	item["branch_logical_bytes"] = storageBranchAdmissionLimit()
	jsonResponse(w, 200, item)
}
