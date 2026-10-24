package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

var servicePlanID = regexp.MustCompile(`^pln_[a-f0-9]{24}$`)
var servicePlanRevisionID = regexp.MustCompile(`^plv_[a-f0-9]{24}$`)

func servicePlanFailure(w http.ResponseWriter, r *http.Request, err error) {
	var problem aiProviderProblem
	if errors.As(err, &problem) {
		fail(w, r, problem.status, problem.code, problem.message)
		return
	}
	fail(w, r, 503, "service_plans_unavailable", "Service plan state is temporarily unavailable")
}
func servicePlanReadFailure(w http.ResponseWriter, r *http.Request, err error) {
	if isNoRows(err) {
		fail(w, r, 404, "not_found", "Service plan or organization entitlement not found")
		return
	}
	servicePlanFailure(w, r, err)
}

type servicePlanInput struct {
	Version      *int64 `json:"version"`
	Name         string `json:"name"`
	MaxProjects  int    `json:"max_projects"`
	MaxEndpoints int    `json:"max_endpoints"`
}

func (i servicePlanInput) valid() bool {
	return i.Version != nil && *i.Version >= 0 && *i.Version < 1000000000000 && displayName(i.Name) && i.MaxProjects >= 1 && i.MaxProjects <= 10000 && i.MaxEndpoints >= 1 && i.MaxEndpoints <= 10000
}

func (s *server) servicePlanMutation(w http.ResponseWriter, r *http.Request, org, plan string, input any, fn func(pgx.Tx, string) (record, int, error)) {
	if !s.requirePlatformPlans(w, r, org) {
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	if len(s.idempotencyKey) < 32 {
		fail(w, r, 503, "platform_plan_key_unavailable", "Plan mutation receipt key is unavailable")
		return
	}
	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		servicePlanFailure(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	actor, hash := userFrom(r).ID, s.createRequestHash(r.Method+":"+r.URL.Path, input)
	_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('service-plan-mutation:'||$1||':'||$2,0))`, actor, keyHash(key))
	if err != nil {
		servicePlanFailure(w, r, err)
		return
	}
	var stored string
	var response []byte
	var status int
	err = tx.QueryRow(ctx, `SELECT request_hash,response,http_status FROM service_plan_mutations WHERE actor_id=$1 AND key_hash=$2`, actor, keyHash(key)).Scan(&stored, &response, &status)
	if err == nil {
		if stored != hash {
			fail(w, r, 409, "idempotency_conflict", "Key was used for different plan data")
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(response)
		return
	}
	if !isNoRows(err) {
		servicePlanFailure(w, r, err)
		return
	}
	id := newID("plm_")
	value, status, err := fn(tx, id)
	if err != nil {
		servicePlanFailure(w, r, err)
		return
	}
	response, err = json.Marshal(value)
	if err != nil || len(response) > 65536 {
		servicePlanFailure(w, r, errors.New("plan receipt exceeds bound"))
		return
	}
	_, err = tx.Exec(ctx, `INSERT INTO service_plan_mutations(id,actor_id,org_id,plan_id,key_hash,request_hash,response,http_status) VALUES($1,$2,NULLIF($3,''),NULLIF($4,''),$5,$6,$7,$8)`, id, actor, org, plan, keyHash(key), hash, response, status)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		servicePlanFailure(w, r, err)
		return
	}
	jsonResponse(w, status, value)
}

func (s *server) servicePlanCatalog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	operator := strings.HasPrefix(r.URL.Path, "/api/v1/platform/")
	if operator && !s.requirePlatformPlans(w, r, "") {
		return
	}
	if id := r.PathValue("servicePlan"); id != "" {
		if !servicePlanID.MatchString(id) {
			fail(w, r, 404, "not_found", "Service plan not found")
			return
		}
		plan, err := s.one(r.Context(), `SELECT id,name,state,version,created_at,updated_at FROM service_plans WHERE id=$1 AND ($2 OR state='active')`, id, operator)
		if err != nil {
			servicePlanReadFailure(w, r, err)
			return
		}
		revisions, err := s.many(r.Context(), `SELECT id,plan_id,revision,max_projects,max_endpoints,created_at FROM service_plan_revisions WHERE plan_id=$1 ORDER BY revision DESC LIMIT 100`, id)
		if err != nil {
			servicePlanFailure(w, r, err)
			return
		}
		jsonResponse(w, 200, record{"plan": plan, "revisions": revisions})
		return
	}
	items, err := s.many(r.Context(), `SELECT p.id,p.name,p.state,p.version,p.created_at,p.updated_at,to_jsonb(r)-'created_by' AS latest_revision FROM service_plans p JOIN LATERAL(SELECT id,plan_id,revision,max_projects,max_endpoints,created_at FROM service_plan_revisions WHERE plan_id=p.id ORDER BY revision DESC LIMIT 1)r ON true WHERE $1 OR p.state='active' ORDER BY p.created_at,p.id LIMIT 100`, operator)
	if err != nil {
		servicePlanFailure(w, r, err)
		return
	}
	jsonResponse(w, 200, record{"items": items, "can_manage": s.platform.permits(userFrom(r), ""), "payment_processing": false, "automatic_credit_grants": false})
}

func (s *server) servicePlanPublish(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformPlans(w, r, "") {
		return
	}
	var input servicePlanInput
	if readJSON(r, &input) != nil || !input.valid() {
		fail(w, r, 422, "invalid_service_plan", "Use a current version, a plan name and resource limits in 1-10000")
		return
	}
	id := r.PathValue("servicePlan")
	creating := id == ""
	if creating {
		if *input.Version != 0 {
			fail(w, r, 422, "invalid_service_plan_version", "A new plan must start at version zero")
			return
		}
		id = newID("pln_")
	} else if !servicePlanID.MatchString(id) || *input.Version < 1 {
		fail(w, r, 422, "invalid_service_plan_version", "Use the plan's current version")
		return
	}
	s.servicePlanMutation(w, r, "", id, input, func(tx pgx.Tx, _ string) (record, int, error) {
		ctx := r.Context()
		actor := userFrom(r).ID
		if creating {
			_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(794210048)`)
			if err != nil {
				return nil, 0, err
			}
			var count int
			if err = tx.QueryRow(ctx, `SELECT count(*) FROM service_plans`).Scan(&count); err != nil {
				return nil, 0, err
			}
			if count >= 100 {
				return nil, 0, providerProblem(429, "service_plan_catalog_full", "At most 100 retained plan definitions are allowed")
			}
			_, err = tx.Exec(ctx, `INSERT INTO service_plans(id,name,state,version,created_by) VALUES($1,$2,'active',1,$3)`, id, input.Name, actor)
			if err != nil {
				return nil, 0, err
			}
		} else {
			var version int64
			var state string
			err := tx.QueryRow(ctx, `SELECT version,state FROM service_plans WHERE id=$1 FOR UPDATE`, id).Scan(&version, &state)
			if isNoRows(err) {
				return nil, 0, providerProblem(404, "not_found", "Service plan not found")
			}
			if err != nil {
				return nil, 0, err
			}
			if version != *input.Version {
				return nil, 0, providerProblem(409, "service_plan_version_conflict", "Plan changed; refresh before publishing")
			}
			if state != "active" {
				return nil, 0, providerProblem(409, "service_plan_disabled", "Enable the plan before publishing a new revision")
			}
			_, err = tx.Exec(ctx, `UPDATE service_plans SET name=$2,version=version+1,updated_at=clock_timestamp() WHERE id=$1`, id, input.Name)
			if err != nil {
				return nil, 0, err
			}
		}
		var next int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(revision),0)+1 FROM service_plan_revisions WHERE plan_id=$1`, id).Scan(&next); err != nil {
			return nil, 0, err
		}
		revision := newID("plv_")
		_, err := tx.Exec(ctx, `INSERT INTO service_plan_revisions(id,plan_id,revision,max_projects,max_endpoints,created_by) VALUES($1,$2,$3,$4,$5,$6)`, revision, id, next, input.MaxProjects, input.MaxEndpoints, actor)
		status := 200
		if creating {
			status = 201
		}
		return record{"plan_id": id, "revision_id": revision, "revision": next, "version": *input.Version + 1, "name": input.Name, "max_projects": input.MaxProjects, "max_endpoints": input.MaxEndpoints, "state": "active"}, status, err
	})
}

func (s *server) servicePlanState(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlatformPlans(w, r, "") {
		return
	}
	var input struct {
		Version *int64 `json:"version"`
		State   string `json:"state"`
	}
	id := r.PathValue("servicePlan")
	if readJSON(r, &input) != nil || input.Version == nil || *input.Version < 1 || *input.Version >= 1000000000000 || !servicePlanID.MatchString(id) || input.State != "active" && input.State != "disabled" {
		fail(w, r, 422, "invalid_service_plan_state", "Use a current version and active/disabled state")
		return
	}
	s.servicePlanMutation(w, r, "", id, input, func(tx pgx.Tx, _ string) (record, int, error) {
		var version int64
		if err := tx.QueryRow(r.Context(), `SELECT version FROM service_plans WHERE id=$1 FOR UPDATE`, id).Scan(&version); err != nil {
			if isNoRows(err) {
				return nil, 0, providerProblem(404, "not_found", "Service plan not found")
			}
			return nil, 0, err
		}
		if version != *input.Version {
			return nil, 0, providerProblem(409, "service_plan_version_conflict", "Plan changed; refresh before changing state")
		}
		_, err := tx.Exec(r.Context(), `UPDATE service_plans SET state=$2,version=version+1,updated_at=clock_timestamp() WHERE id=$1`, id, input.State)
		return record{"plan_id": id, "state": input.State, "version": version + 1, "existing_assignments_preserved": true}, 200, err
	})
}

func (s *server) organizationServicePlan(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	if org == "" {
		org = r.PathValue("planOrg")
		if !s.requirePlatformPlans(w, r, org) {
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == "GET" {
		value, err := s.one(r.Context(), `SELECT q.org_id,q.max_projects,q.max_endpoints,COALESCE(b.version,0) AS version,b.revision_id,r.plan_id,p.name AS plan_name,COALESCE(p.state,'legacy') AS plan_state,COALESCE(b.base_max_projects,q.max_projects) AS base_max_projects,COALESCE(b.base_max_endpoints,q.max_endpoints) AS base_max_endpoints,(SELECT count(*) FROM projects x WHERE x.org_id=q.org_id AND x.deleted_at IS NULL) AS current_projects,(SELECT count(*) FROM endpoints e JOIN projects x ON x.id=e.project_id WHERE x.org_id=q.org_id AND e.deleted_at IS NULL) AS current_endpoints FROM organization_quotas q LEFT JOIN organization_service_plans b ON b.org_id=q.org_id LEFT JOIN service_plan_revisions r ON r.id=b.revision_id LEFT JOIN service_plans p ON p.id=r.plan_id WHERE q.org_id=$1`, org)
		if err != nil {
			servicePlanReadFailure(w, r, err)
			return
		}
		history, err := s.many(r.Context(), `SELECT id,revision_id,previous_revision_id,version,max_projects,max_endpoints,created_at FROM organization_service_plan_history WHERE org_id=$1 ORDER BY version DESC LIMIT 100`, org)
		if err != nil {
			servicePlanFailure(w, r, err)
			return
		}
		jsonResponse(w, 200, record{"entitlement": value, "history": history, "can_manage": s.platform.permits(userFrom(r), org), "payment_processing": false})
		return
	}
	var input struct {
		Version  *int64          `json:"version"`
		Revision json.RawMessage `json:"revision_id"`
	}
	if readJSON(r, &input) != nil || input.Version == nil || *input.Version < 0 || *input.Version >= 1000000000000 || len(input.Revision) == 0 {
		fail(w, r, 422, "invalid_service_plan_assignment", "Use the current assignment version and an explicit revision_id (or null for original quotas)")
		return
	}
	revision := ""
	if !bytes.Equal(bytes.TrimSpace(input.Revision), []byte("null")) {
		if json.Unmarshal(input.Revision, &revision) != nil || !servicePlanRevisionID.MatchString(revision) {
			fail(w, r, 422, "invalid_service_plan_revision", "Use an immutable published plan revision")
			return
		}
	}
	s.servicePlanMutation(w, r, org, "", input, func(tx pgx.Tx, mutation string) (record, int, error) {
		return s.assignOrganizationServicePlan(r.Context(), tx, org, revision, *input.Version, userFrom(r).ID, mutation)
	})
}

func (s *server) assignOrganizationServicePlan(ctx context.Context, tx pgx.Tx, org, revision string, expectedVersion int64, actor, mutation string) (record, int, error) {
	maxProjects, maxEndpoints := 0, 0
	if revision != "" {
		var state string
		err := tx.QueryRow(ctx, `SELECT p.state,r.max_projects,r.max_endpoints FROM service_plan_revisions r JOIN service_plans p ON p.id=r.plan_id WHERE r.id=$1 FOR SHARE OF p`, revision).Scan(&state, &maxProjects, &maxEndpoints)
		if isNoRows(err) {
			return nil, 0, providerProblem(404, "not_found", "Plan revision not found")
		}
		if err != nil {
			return nil, 0, err
		}
		if state != "active" {
			return nil, 0, providerProblem(409, "service_plan_disabled", "Disabled plans cannot receive new assignments")
		}
	}
	var baseProjects, baseEndpoints int
	err := tx.QueryRow(ctx, `SELECT max_projects,max_endpoints FROM organization_quotas WHERE org_id=$1 FOR UPDATE`, org).Scan(&baseProjects, &baseEndpoints)
	if isNoRows(err) {
		return nil, 0, providerProblem(404, "not_found", "Organization quota record not found")
	}
	if err != nil {
		return nil, 0, err
	}
	var version int64
	var previous *string
	err = tx.QueryRow(ctx, `SELECT version,revision_id,base_max_projects,base_max_endpoints FROM organization_service_plans WHERE org_id=$1 FOR UPDATE`, org).Scan(&version, &previous, &baseProjects, &baseEndpoints)
	if err != nil && !isNoRows(err) {
		return nil, 0, err
	}
	if version != expectedVersion {
		return nil, 0, providerProblem(409, "organization_plan_version_conflict", "Organization plan changed; refresh before assigning")
	}
	if revision == "" {
		maxProjects, maxEndpoints = baseProjects, baseEndpoints
	}
	var projects, endpoints int
	err = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM projects WHERE org_id=$1 AND deleted_at IS NULL),(SELECT count(*) FROM endpoints e JOIN projects p ON p.id=e.project_id WHERE p.org_id=$1 AND e.deleted_at IS NULL)`, org).Scan(&projects, &endpoints)
	if err != nil {
		return nil, 0, err
	}
	if projects > maxProjects || endpoints > maxEndpoints {
		return nil, 0, providerProblem(409, "service_plan_below_current_usage", "The selected plan is below current resource usage; remove resources or choose a larger plan")
	}
	_, err = tx.Exec(ctx, `INSERT INTO organization_service_plans(org_id,revision_id,version,base_max_projects,base_max_endpoints,created_by,updated_by) VALUES($1,NULLIF($2,''),1,$3,$4,$5,$5) ON CONFLICT(org_id) DO UPDATE SET revision_id=EXCLUDED.revision_id,version=organization_service_plans.version+1,updated_by=EXCLUDED.updated_by,updated_at=clock_timestamp()`, org, revision, baseProjects, baseEndpoints, actor)
	if err != nil {
		return nil, 0, err
	}
	_, err = tx.Exec(ctx, `UPDATE organization_quotas SET max_projects=$2,max_endpoints=$3 WHERE org_id=$1`, org, maxProjects, maxEndpoints)
	if err != nil {
		return nil, 0, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO organization_service_plan_history(id,mutation_id,org_id,revision_id,previous_revision_id,version,max_projects,max_endpoints,actor_id) VALUES($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9)`, newID("plh_"), mutation, org, revision, previous, version+1, maxProjects, maxEndpoints, actor)
	value := record{"org_id": org, "revision_id": revision, "version": version + 1, "max_projects": maxProjects, "max_endpoints": maxEndpoints, "existing_resources_changed": false, "payment_collected": false}
	if revision == "" {
		value["revision_id"] = nil
	}
	return value, 200, err
}
