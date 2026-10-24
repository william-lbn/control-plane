-- Immutable self-hosted resource entitlements. Plans do not imply that a
-- payment, subscription renewal or external money transfer has taken place.
CREATE TABLE service_plans (
 id text PRIMARY KEY CHECK(id ~ '^pln_[a-f0-9]{24}$'),
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 80),
 state text NOT NULL CHECK(state IN ('active','disabled')),
 version bigint NOT NULL CHECK(version BETWEEN 1 AND 1000000000000),
 created_by text NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE service_plan_revisions (
 id text PRIMARY KEY CHECK(id ~ '^plv_[a-f0-9]{24}$'),
 plan_id text NOT NULL REFERENCES service_plans(id),
 revision bigint NOT NULL CHECK(revision BETWEEN 1 AND 1000000000000),
 max_projects integer NOT NULL CHECK(max_projects BETWEEN 1 AND 10000),
 max_endpoints integer NOT NULL CHECK(max_endpoints BETWEEN 1 AND 10000),
 created_by text NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(plan_id,revision),
 UNIQUE(id,plan_id)
);
CREATE TABLE organization_service_plans (
 org_id text PRIMARY KEY REFERENCES organization_quotas(org_id),
 revision_id text REFERENCES service_plan_revisions(id),
 version bigint NOT NULL CHECK(version BETWEEN 1 AND 1000000000000),
 base_max_projects integer NOT NULL CHECK(base_max_projects>0),
 base_max_endpoints integer NOT NULL CHECK(base_max_endpoints>0),
 created_by text NOT NULL REFERENCES users(id),
 updated_by text NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE service_plan_mutations (
 id text PRIMARY KEY CHECK(id ~ '^plm_[a-f0-9]{24}$'),
 actor_id text NOT NULL REFERENCES users(id),
 org_id text REFERENCES organizations(id),
 plan_id text REFERENCES service_plans(id),
 key_hash text NOT NULL,
 request_hash text NOT NULL,
 response jsonb NOT NULL CHECK(jsonb_typeof(response)='object' AND octet_length(response::text)<=65536),
 http_status integer NOT NULL CHECK(http_status IN (200,201)),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(actor_id,key_hash)
);
CREATE TABLE organization_service_plan_history (
 id text PRIMARY KEY CHECK(id ~ '^plh_[a-f0-9]{24}$'),
 mutation_id text NOT NULL UNIQUE REFERENCES service_plan_mutations(id) DEFERRABLE INITIALLY DEFERRED,
 org_id text NOT NULL REFERENCES organizations(id),
 revision_id text REFERENCES service_plan_revisions(id),
 previous_revision_id text REFERENCES service_plan_revisions(id),
 version bigint NOT NULL CHECK(version>0),
 max_projects integer NOT NULL CHECK(max_projects>0),
 max_endpoints integer NOT NULL CHECK(max_endpoints>0),
 actor_id text NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(org_id,version)
);
CREATE TRIGGER service_plan_revision_guard BEFORE UPDATE OR DELETE ON service_plan_revisions FOR EACH ROW EXECUTE FUNCTION guard_ai_input_receipt();
CREATE TRIGGER service_plan_mutation_guard BEFORE UPDATE OR DELETE ON service_plan_mutations FOR EACH ROW EXECUTE FUNCTION guard_ai_input_receipt();
CREATE TRIGGER organization_service_plan_history_guard BEFORE UPDATE OR DELETE ON organization_service_plan_history FOR EACH ROW EXECUTE FUNCTION guard_ai_input_receipt();
CREATE FUNCTION guard_service_plan() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'Service plan history is retained'; END IF;
 IF (NEW.id,NEW.created_by,NEW.created_at) IS DISTINCT FROM (OLD.id,OLD.created_by,OLD.created_at) OR NEW.version<>OLD.version+1 OR NEW.updated_at<OLD.updated_at THEN RAISE EXCEPTION 'Invalid service plan update'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER service_plan_guard BEFORE UPDATE OR DELETE ON service_plans FOR EACH ROW EXECUTE FUNCTION guard_service_plan();
CREATE FUNCTION guard_organization_service_plan() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'Service plan binding history is retained'; END IF;
 IF (NEW.org_id,NEW.base_max_projects,NEW.base_max_endpoints,NEW.created_by,NEW.created_at) IS DISTINCT FROM (OLD.org_id,OLD.base_max_projects,OLD.base_max_endpoints,OLD.created_by,OLD.created_at) OR NEW.version<>OLD.version+1 OR NEW.updated_at<OLD.updated_at THEN RAISE EXCEPTION 'Invalid service plan binding update'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER organization_service_plan_guard BEFORE UPDATE OR DELETE ON organization_service_plans FOR EACH ROW EXECUTE FUNCTION guard_organization_service_plan();
CREATE FUNCTION guard_service_plan_quota() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE projects_limit integer; endpoints_limit integer;
BEGIN
 SELECT COALESCE(r.max_projects,b.base_max_projects),COALESCE(r.max_endpoints,b.base_max_endpoints) INTO projects_limit,endpoints_limit FROM organization_service_plans b LEFT JOIN service_plan_revisions r ON r.id=b.revision_id WHERE b.org_id=NEW.org_id;
 IF FOUND AND (NEW.max_projects<>projects_limit OR NEW.max_endpoints<>endpoints_limit) THEN RAISE EXCEPTION 'Bound service plan quotas must match their immutable revision'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER service_plan_quota_guard BEFORE UPDATE OF max_projects,max_endpoints ON organization_quotas FOR EACH ROW EXECUTE FUNCTION guard_service_plan_quota();
CREATE FUNCTION guard_service_plan_binding_commit() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE actual_projects integer; actual_endpoints integer; expected_projects integer; expected_endpoints integer;
BEGIN
 SELECT q.max_projects,q.max_endpoints,COALESCE(r.max_projects,b.base_max_projects),COALESCE(r.max_endpoints,b.base_max_endpoints) INTO actual_projects,actual_endpoints,expected_projects,expected_endpoints FROM organization_quotas q JOIN organization_service_plans b ON b.org_id=q.org_id LEFT JOIN service_plan_revisions r ON r.id=b.revision_id WHERE q.org_id=NEW.org_id;
 IF FOUND AND (actual_projects<>expected_projects OR actual_endpoints<>expected_endpoints) THEN RAISE EXCEPTION 'Service plan binding and effective quotas must commit atomically'; END IF;
 RETURN NEW;
END $$;
CREATE CONSTRAINT TRIGGER service_plan_binding_commit_guard AFTER INSERT OR UPDATE ON organization_service_plans DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION guard_service_plan_binding_commit();
CREATE CONSTRAINT TRIGGER service_plan_quota_commit_guard AFTER INSERT OR UPDATE ON organization_quotas DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION guard_service_plan_binding_commit();
