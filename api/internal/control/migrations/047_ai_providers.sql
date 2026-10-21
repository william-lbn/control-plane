-- Forward-only provider catalog. Credentials are scoped sealed boxes; public
-- read handlers never select them. Static operator routes remain independent.
CREATE TABLE ai_provider_catalog_state (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 revision integer NOT NULL DEFAULT 0 CHECK(revision>=0 AND revision<1000000000)
);
INSERT INTO ai_provider_catalog_state(singleton) VALUES(true);
CREATE TABLE ai_providers (
 id text PRIMARY KEY CHECK(id ~ '^aip_[a-f0-9]{24}$'),
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 100),
 version bigint NOT NULL DEFAULT 1 CHECK(version>0),
 state text NOT NULL DEFAULT 'draft' CHECK(state IN ('draft','active','disabled','deleted')),
 active_publication_id text,
 target_revision_id text,
 created_by text NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(state<>'active' OR active_publication_id IS NOT NULL)
);
CREATE TABLE ai_provider_revisions (
 id text PRIMARY KEY CHECK(id ~ '^apv_[a-f0-9]{24}$'),
 provider_id text NOT NULL REFERENCES ai_providers(id),
 base_url text NOT NULL CHECK(length(base_url) BETWEEN 1 AND 2048),
 endpoint_sha256 text NOT NULL CHECK(endpoint_sha256 ~ '^[a-f0-9]{64}$'),
 credential_mode text NOT NULL CHECK(credential_mode IN ('none','sealed','operator_reference','reuse')),
 credential_source_id text,
 previous_revision_id text,
 credential_box jsonb,
 source_secret_uid text,
 network jsonb,
 models jsonb,
 policy_sha256 text NOT NULL CHECK(policy_sha256 ~ '^[a-f0-9]{64}$'),
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','tested','failed')),
 tested_at timestamptz,
 created_by text NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(provider_id,id),
 FOREIGN KEY(provider_id,previous_revision_id) REFERENCES ai_provider_revisions(provider_id,id),
 CHECK((credential_mode='operator_reference')=(credential_source_id IS NOT NULL)),
 CHECK((credential_mode='reuse')=(previous_revision_id IS NOT NULL)),
 CHECK(credential_mode<>'none' OR credential_box IS NULL),
 CHECK(credential_mode<>'sealed' OR credential_box IS NOT NULL),
 CHECK(credential_box IS NULL OR (jsonb_typeof(credential_box)='object' AND octet_length(credential_box::text)<20000)),
 CHECK(state<>'tested' OR (network IS NOT NULL AND models IS NOT NULL AND tested_at IS NOT NULL AND (credential_mode='none' OR credential_box IS NOT NULL))),
 CHECK(models IS NULL OR (jsonb_typeof(models)='array' AND jsonb_array_length(models)<=256))
);
ALTER TABLE ai_providers ADD FOREIGN KEY(id,target_revision_id) REFERENCES ai_provider_revisions(provider_id,id) DEFERRABLE INITIALLY DEFERRED;
CREATE TABLE ai_provider_operations (
 id text PRIMARY KEY CHECK(id ~ '^apo_[a-f0-9]{24}$'),
 provider_id text NOT NULL,
 revision_id text NOT NULL UNIQUE,
 actor_id text NOT NULL REFERENCES users(id),
 state text NOT NULL DEFAULT 'queued' CHECK(state IN ('queued','running','succeeded','failed')),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts>=0),
 lease_owner text,
 lease_expires_at timestamptz,
 retryable boolean NOT NULL DEFAULT false,
 error_code text,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 finished_at timestamptz,
 FOREIGN KEY(provider_id,revision_id) REFERENCES ai_provider_revisions(provider_id,id)
);
CREATE INDEX ai_provider_operations_work ON ai_provider_operations(created_at) WHERE state IN ('queued','running');
CREATE TABLE ai_provider_publications (
 id text PRIMARY KEY CHECK(id ~ '^app_[a-f0-9]{24}$'),
 provider_id text NOT NULL,
 revision_id text NOT NULL,
 models jsonb NOT NULL CHECK(jsonb_typeof(models)='array' AND jsonb_array_length(models) BETWEEN 1 AND 16),
 created_by text NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(provider_id,id),
 FOREIGN KEY(provider_id,revision_id) REFERENCES ai_provider_revisions(provider_id,id)
);
ALTER TABLE ai_providers ADD FOREIGN KEY(id,active_publication_id) REFERENCES ai_provider_publications(provider_id,id) DEFERRABLE INITIALLY DEFERRED;
CREATE TABLE ai_provider_requests (
 actor_id text NOT NULL REFERENCES users(id),
 key_hash text NOT NULL,
 request_hash text NOT NULL,
 status integer NOT NULL CHECK(status IN (200,201,202)),
 response jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(actor_id,key_hash)
);

CREATE FUNCTION guard_ai_provider_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'provider revision history is retained'; END IF;
 IF (NEW.id,NEW.provider_id,NEW.base_url,NEW.endpoint_sha256,NEW.credential_mode,NEW.credential_source_id,NEW.previous_revision_id,NEW.policy_sha256,NEW.created_by,NEW.created_at)
   IS DISTINCT FROM (OLD.id,OLD.provider_id,OLD.base_url,OLD.endpoint_sha256,OLD.credential_mode,OLD.credential_source_id,OLD.previous_revision_id,OLD.policy_sha256,OLD.created_by,OLD.created_at)
   OR OLD.credential_box IS NOT NULL AND NEW.credential_box IS DISTINCT FROM OLD.credential_box
   OR OLD.source_secret_uid IS NOT NULL AND NEW.source_secret_uid IS DISTINCT FROM OLD.source_secret_uid
   OR OLD.state='tested' AND NEW IS DISTINCT FROM OLD THEN
  RAISE EXCEPTION 'provider revision inputs or tested receipt are immutable';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER ai_provider_revision_guard BEFORE UPDATE OR DELETE ON ai_provider_revisions FOR EACH ROW EXECUTE FUNCTION guard_ai_provider_revision();
CREATE FUNCTION guard_ai_provider_publication() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'provider publication history is immutable';
END $$;
CREATE TRIGGER ai_provider_publication_guard BEFORE UPDATE OR DELETE ON ai_provider_publications FOR EACH ROW EXECUTE FUNCTION guard_ai_provider_publication();
-- Grant SELECT on this view plus catalog_state to the existing restricted
-- Gateway role during operator deployment; do not grant base-table writes.
CREATE VIEW ai_provider_runtime AS
 SELECT p.id AS provider_id, r.id AS revision_id, r.base_url,r.endpoint_sha256,
 r.credential_mode,r.credential_box,r.network,r.policy_sha256,pub.models
 FROM ai_providers p JOIN ai_provider_publications pub ON pub.id=p.active_publication_id AND pub.provider_id=p.id
 JOIN ai_provider_revisions r ON r.id=pub.revision_id AND r.provider_id=p.id
 WHERE p.state='active' AND r.state='tested';
