package control

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"

	"github.com/william-lbn/control-plane/api/internal/functions"
)

// These read routes expose durable, authorized metadata only. They never
// cold-wake SQL, poll a guest, read a Secret or synthesize runtime readiness.
// Functions execution stays disabled until the deployment Driver is admitted.
const functionColumns = `id,project_id,branch_id,slug,database_name,sql_schema,version,generation,state,
 active_deployment_id,target_deployment_id,idle_timeout_seconds,created_at,updated_at`
const deploymentColumns = `id,function_id,project_id,branch_id,runtime,bundle_digest,bundle_bytes,entry,
 environment_names,state,operation_id,failure_code,created_at,finished_at`

var functionDeploymentID = regexp.MustCompile(`^fdp_[a-f0-9]{16}$`)

func (s *server) functionReadBranch(w http.ResponseWriter, r *http.Request) bool {
	var exists bool
	err := s.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM branches WHERE id=$1 AND project_id=$2 AND deleted_at IS NULL)`, r.PathValue("branch"), r.PathValue("project")).Scan(&exists)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read Functions branch")
		return false
	}
	if !exists {
		fail(w, r, 404, "not_found", "Branch not found")
		return false
	}
	return true
}
func functionReadLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	limit := 50
	if text := r.URL.Query().Get("limit"); text != "" {
		n, err := strconv.Atoi(text)
		if err != nil || n < 1 || n > 100 {
			fail(w, r, 422, "invalid_page", "Limit must be between 1 and 100")
			return 0, false
		}
		limit = n
	}
	return limit, true
}
func (s *server) functionsList(w http.ResponseWriter, r *http.Request) {
	if !s.functionReadBranch(w, r) {
		return
	}
	limit, ok := functionReadLimit(w, r)
	if !ok {
		return
	}
	after := r.URL.Query().Get("after")
	if after != "" && !functions.ValidSlug(after) {
		fail(w, r, 422, "invalid_page", "Invalid function slug cursor")
		return
	}
	items, err := s.many(r.Context(), `SELECT `+functionColumns+` FROM function_definitions WHERE project_id=$1 AND branch_id=$2 AND state<>'deleted' AND slug>$3 ORDER BY slug LIMIT $4`, r.PathValue("project"), r.PathValue("branch"), after, limit+1)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read Functions")
		return
	}
	var next any
	if len(items) > limit {
		items = items[:limit]
		next = items[len(items)-1]["slug"]
	}
	for _, item := range items {
		functionReadSummary(item)
	}
	jsonResponse(w, 200, record{"items": items, "next_cursor": next, "driver_enabled": false})
}
func functionReadSummary(item record) {
	// A deployment can be retained or have failed since the last VM observation.
	// Never invent a public invocation URL or infer Node readiness from state.
	item["invocation_url"] = nil
	item["runtime_observed"] = false
	item["driver_enabled"] = false
}
func (s *server) functionRead(w http.ResponseWriter, r *http.Request) {
	if !s.functionReadBranch(w, r) {
		return
	}
	if !functions.ValidSlug(r.PathValue("slug")) {
		fail(w, r, 404, "not_found", "Function not found")
		return
	}
	item, err := s.one(r.Context(), `SELECT `+functionColumns+` FROM function_definitions WHERE project_id=$1 AND branch_id=$2 AND slug=$3 AND state<>'deleted'`, r.PathValue("project"), r.PathValue("branch"), r.PathValue("slug"))
	if err != nil {
		if isNoRows(err) {
			fail(w, r, 404, "not_found", "Function not found")
		} else {
			fail(w, r, 503, "metadata_unavailable", "Could not read Function")
		}
		return
	}
	functionReadSummary(item)
	w.Header().Set("ETag", fmt.Sprintf(`"%d"`, item["version"].(int64)))
	jsonResponse(w, 200, item)
}
func (s *server) functionDeployments(w http.ResponseWriter, r *http.Request) {
	if !s.functionReadBranch(w, r) {
		return
	}
	if !functions.ValidSlug(r.PathValue("slug")) {
		fail(w, r, 404, "not_found", "Function not found")
		return
	}
	limit, ok := functionReadLimit(w, r)
	if !ok {
		return
	}
	after := r.URL.Query().Get("after")
	if after != "" && !functionDeploymentID.MatchString(after) {
		fail(w, r, 422, "invalid_page", "Invalid deployment cursor")
		return
	}
	definition, err := s.one(r.Context(), `SELECT id FROM function_definitions WHERE project_id=$1 AND branch_id=$2 AND slug=$3 AND state<>'deleted'`, r.PathValue("project"), r.PathValue("branch"), r.PathValue("slug"))
	if err != nil {
		if isNoRows(err) {
			fail(w, r, 404, "not_found", "Function not found")
		} else {
			fail(w, r, 503, "metadata_unavailable", "Could not read Function")
		}
		return
	}
	// IDs are immutable random identities, not timestamps. Stable ID ordering
	// makes the opaque cursor unambiguous under concurrent deployment insertion.
	items, err := s.many(r.Context(), `SELECT `+deploymentColumns+` FROM function_deployments WHERE function_id=$1 AND project_id=$2 AND branch_id=$3 AND id>$4 ORDER BY id LIMIT $5`, definition["id"], r.PathValue("project"), r.PathValue("branch"), after, limit+1)
	if err != nil {
		fail(w, r, 503, "metadata_unavailable", "Could not read deployments")
		return
	}
	var next any
	if len(items) > limit {
		items = items[:limit]
		next = items[len(items)-1]["id"]
	}
	jsonResponse(w, 200, record{"items": items, "next_cursor": next})
}
