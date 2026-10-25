package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestPlatformPolicyExplicitApprovalAndScope(t *testing.T) {
	p, err := newPlatformPolicy([]byte(`{"version":1,"operator_ids":["approved"],"allowed_organizations":["org_owned"],"plan_management":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !p.permits(user{ID: "approved"}, "org_owned") {
		t.Fatal("approved operator denied")
	}
	for _, u := range []user{{ID: "approved", APIKey: true}, {ID: "other", Role: "owner"}, {ID: "other", Role: "admin"}} {
		if p.permits(u, "org_owned") {
			t.Fatal("role or API key gained operator powers")
		}
	}
	if p.permits(user{ID: "approved"}, "org_other") {
		t.Fatal("organization allowlist ignored")
	}
	var absent *platformPolicy
	if absent.permits(user{ID: "approved"}, "org_owned") {
		t.Fatal("missing approval enabled platform management")
	}
	for _, raw := range []string{`{}`, `{"version":1,"operator_ids":[]}`, `{"version":1,"operator_ids":["x","x"]}`, `{"version":1,"operator_ids":["x"],"secret":"unexpected"}`, `{"version":1,"operator_ids":["x"],"allowed_organizations":["a","a"]}`, strings.Repeat("x", 65537)} {
		if _, err := newPlatformPolicy([]byte(raw)); err == nil {
			t.Fatal("unsafe approval policy accepted")
		}
	}
	p, err = newPlatformPolicy([]byte(`{"version":1,"operator_ids":["approved"]}`))
	if err != nil || p.permits(user{ID: "approved"}, "") {
		t.Fatal("plan approval was inferred from omitted flag")
	}
}

const platformPlanRoot = "/api/v1/platform/service-plans"
const platformPlanOrg = "/api/v1/platform/organizations/org_provider/service-plan"

func newServicePlanFixture(t *testing.T) *providerFixture {
	f := newProviderFixture(t)
	var err error
	f.s.platform, err = newPlatformPolicy([]byte(`{"version":1,"operator_ids":["provider_operator"],"allowed_organizations":["org_provider"],"plan_management":true}`))
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO organization_quotas(org_id) VALUES('org_provider') ON CONFLICT DO NOTHING`)
	return f
}
func publishServicePlan(t *testing.T, f *providerFixture, key string, projects, endpoints int) record {
	t.Helper()
	return f.call("provider_operator", "POST", platformPlanRoot, key, record{"version": 0, "name": "Synthetic service plan", "max_projects": projects, "max_endpoints": endpoints}, 201)
}
func servicePlanQuotas(t *testing.T, f *providerFixture, projects, endpoints int) {
	t.Helper()
	var a, b int
	if err := f.s.db.QueryRow(f.ctx, `SELECT max_projects,max_endpoints FROM organization_quotas WHERE org_id='org_provider'`).Scan(&a, &b); err != nil || a != projects || b != endpoints {
		t.Fatal("effective quota differs from selected immutable plan", a, b, err)
	}
}

func TestServicePlanImmutablePublicationReplayAndLegacyRestore(t *testing.T) {
	f := newServicePlanFixture(t)
	plan := publishServicePlan(t, f, "plan-original-create", 2, 3)
	replayed := f.request("provider_operator", "POST", platformPlanRoot, "plan-original-create", record{"version": 0, "name": "Synthetic service plan", "max_projects": 2, "max_endpoints": 3})
	if replayed.Code != 201 || replayed.Header().Get("Idempotency-Replayed") != "true" || !strings.Contains(replayed.Body.String(), plan["plan_id"].(string)) {
		t.Fatal("plan create did not replay original receipt")
	}
	f.call("provider_operator", "POST", platformPlanRoot, "plan-original-create", record{"version": 0, "name": "Changed", "max_projects": 9, "max_endpoints": 9}, 409)
	f.call("provider_operator", "PUT", platformPlanOrg, "plan-original-assign", record{"version": 0, "revision_id": plan["revision_id"]}, 200)
	servicePlanQuotas(t, f, 2, 3)
	changed := f.call("provider_operator", "POST", platformPlanRoot+"/"+plan["plan_id"].(string)+"/revisions", "plan-publish-next", record{"version": 1, "name": "Revised service plan", "max_projects": 5, "max_endpoints": 6}, 200)
	servicePlanQuotas(t, f, 2, 3)
	current := f.call("provider_operator", "GET", "/api/v1/organizations/org_provider/service-plan", "", nil, 200)["entitlement"].(map[string]any)
	if current["revision_id"] != plan["revision_id"] {
		t.Fatal("publishing silently moved an existing entitlement")
	}
	f.call("provider_operator", "PUT", platformPlanOrg, "plan-explicit-switch", record{"version": 1, "revision_id": changed["revision_id"]}, 200)
	servicePlanQuotas(t, f, 5, 6)
	f.call("provider_operator", "PUT", platformPlanOrg, "plan-restore-original-quotas", record{"version": 2, "revision_id": nil}, 200)
	servicePlanQuotas(t, f, 50, 200)
	for _, sql := range []string{`UPDATE service_plan_revisions SET max_projects=999`, `DELETE FROM service_plan_revisions`, `UPDATE organization_service_plan_history SET max_projects=999`, `DELETE FROM service_plan_mutations`, `UPDATE organization_service_plans SET base_max_projects=999,version=version+1`, `UPDATE organization_quotas SET max_projects=999 WHERE org_id='org_provider'`} {
		if _, err := f.s.db.Exec(f.ctx, sql); err == nil {
			t.Fatal("immutable plan scope/history/quota was changed", sql)
		}
	}
	var count int
	partial, err := f.s.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = partial.Exec(f.ctx, `UPDATE organization_service_plans SET revision_id=$1,version=version+1,updated_at=clock_timestamp() WHERE org_id='org_provider'`, plan["revision_id"]); err != nil {
		t.Fatal(err)
	}
	if err = partial.Commit(f.ctx); err == nil {
		t.Fatal("A binding committed without its matching effective quotas")
	}
	servicePlanQuotas(t, f, 50, 200)
	if err := f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM organization_service_plan_history WHERE org_id='org_provider'`).Scan(&count); err != nil || count != 3 {
		t.Fatal("assignment history lost or duplicated", count, err)
	}
}

func TestServicePlanApprovalDisabledCatalogAndOldAssignments(t *testing.T) {
	f := newServicePlanFixture(t)
	f.call("provider_org_admin", "POST", platformPlanRoot, "unapproved-plan-create", record{"version": 0, "name": "Denied plan", "max_projects": 1, "max_endpoints": 1}, 403)
	plan := publishServicePlan(t, f, "disable-plan-original", 2, 3)
	id := plan["plan_id"].(string)
	f.call("provider_operator", "PUT", platformPlanOrg, "disable-before-assignment", record{"version": 0, "revision_id": plan["revision_id"]}, 200)
	f.call("provider_operator", "PUT", platformPlanRoot+"/"+id+"/state", "disable-source-plan", record{"version": 1, "state": "disabled"}, 200)
	servicePlanQuotas(t, f, 2, 3)
	f.call("provider_operator", "PUT", platformPlanOrg, "disabled-reassignment", record{"version": 1, "revision_id": plan["revision_id"]}, 409)
	active := f.call("provider_org_admin", "GET", "/api/v1/service-plans", "", nil, 200)
	if len(active["items"].([]any)) != 0 {
		t.Fatal("disabled plan remained in public assignment catalog")
	}
	f.call("provider_org_admin", "GET", "/api/v1/service-plans/"+id, "", nil, 404)
	f.call("provider_operator", "GET", platformPlanRoot+"/"+id, "", nil, 200)
	f.call("provider_operator", "PUT", strings.Replace(platformPlanOrg, "org_provider", "org_other", 1), "cross-organization-plan", record{"version": 0, "revision_id": plan["revision_id"]}, 403)
	f.s.platform = nil
	f.call("provider_operator", "POST", platformPlanRoot, "policy-withdrawn-plan", record{"version": 0, "name": "No approval", "max_projects": 1, "max_endpoints": 1}, 503)
	servicePlanQuotas(t, f, 2, 3)
}

func TestServicePlanDowngradeAtomicityAndActualAdmission(t *testing.T) {
	f := newServicePlanFixture(t)
	large := publishServicePlan(t, f, "large-plan-initial", 5, 6)
	small := publishServicePlan(t, f, "small-plan-initial", 1, 1)
	f.call("provider_operator", "PUT", platformPlanOrg, "assign-large-original", record{"version": 0, "revision_id": large["revision_id"]}, 200)
	f.exec(`INSERT INTO projects(id,org_id,name,region_id,postgres_version,state,source,created_at,updated_at) VALUES('prj_service_plan_second','org_provider','Second','lab',16,'creating','managed',now(),now())`)
	f.call("provider_operator", "PUT", platformPlanOrg, "reject-unsafe-downgrade", record{"version": 1, "revision_id": small["revision_id"]}, 409)
	servicePlanQuotas(t, f, 5, 6)
	var count int
	if err := f.s.db.QueryRow(f.ctx, `SELECT count(*) FROM organization_service_plan_history`).Scan(&count); err != nil || count != 1 {
		t.Fatal("rejected downgrade left a partial mutation", err)
	}
	limit := publishServicePlan(t, f, "plan-exact-project-limit", 2, 3)
	f.call("provider_operator", "PUT", platformPlanOrg, "assign-exact-live-limit", record{"version": 1, "revision_id": limit["revision_id"]}, 200)
	tx, err := f.s.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if err = projectQuota(f.ctx, tx, "org_provider"); !errors.Is(err, errOrganizationQuota) {
		t.Fatal("existing project admission did not enforce the plan", err)
	}
}

func TestServicePlanConcurrentVersionAndExplicitClear(t *testing.T) {
	f := newServicePlanFixture(t)
	a := publishServicePlan(t, f, "race-plan-a", 3, 4)
	b := publishServicePlan(t, f, "race-plan-b", 5, 6)
	f.call("provider_operator", "PUT", platformPlanOrg, "missing-explicit-revision", record{"version": 0}, 422)
	codes := make(chan int, 2)
	for i, value := range []record{a, b} {
		go func(i int, value record) {
			codes <- f.request("provider_operator", "PUT", platformPlanOrg, fmt.Sprintf("race-plan-assignment-%d", i), record{"version": 0, "revision_id": value["revision_id"]}).Code
		}(i, value)
	}
	x, y := <-codes, <-codes
	if !(x == 200 && y == 409 || x == 409 && y == 200) {
		t.Fatal("concurrent assignment bypassed CAS", x, y)
	}
	var version int64
	var max, planMax int
	if err := f.s.db.QueryRow(f.ctx, `SELECT b.version,q.max_projects,r.max_projects FROM organization_service_plans b JOIN organization_quotas q ON q.org_id=b.org_id JOIN service_plan_revisions r ON r.id=b.revision_id WHERE b.org_id='org_provider'`).Scan(&version, &max, &planMax); err != nil || version != 1 || max != planMax {
		t.Fatal("assignment and effective quota diverged", version, max, planMax, err)
	}
	value := f.call("provider_operator", "GET", platformPlanOrg, "", nil, 200)
	raw, _ := json.Marshal(value)
	if strings.Contains(string(raw), "payment_collected\":true") {
		t.Fatal("plan assignment claimed a payment")
	}
}
