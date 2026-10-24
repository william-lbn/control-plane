-- Provider-owned state is referenced through platform-owned, branch-scoped
-- handles. Prompts and model replies are never stored in these usage records.
CREATE TABLE ai_provider_handles (
 id text PRIMARY KEY,
 kind text NOT NULL CHECK(kind IN ('response','conversation')),
 project_id text NOT NULL,
 org_id text NOT NULL,
 branch_id text NOT NULL,
 actor_id text NOT NULL REFERENCES users(id),
 model_id text NOT NULL CHECK(length(model_id) BETWEEN 1 AND 128),
 provider_id text NOT NULL,
 provider_revision_id text NOT NULL,
 provider_identity text NOT NULL CHECK(provider_identity ~ '^[a-f0-9]{64}$'),
 key_version text NOT NULL,
 request_id text NOT NULL UNIQUE,
 parent_handle_id text,
 previous_handle_id text,
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','ready','uncertain','deleting','deleted','revoked')),
 provider_status text NOT NULL DEFAULT 'pending' CHECK(provider_status IN ('pending','ready','queued','in_progress','completed','incomplete','failed','cancelled','deleted')),
 remote_identity_sha256 text CHECK(remote_identity_sha256 ~ '^[a-f0-9]{64}$'),
 ciphertext bytea CHECK(octet_length(ciphertext) BETWEEN 28 AND 16412),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 version bigint NOT NULL DEFAULT 1 CHECK(version>0),
 UNIQUE(id,project_id,branch_id),
 FOREIGN KEY(project_id,org_id) REFERENCES projects(id,org_id),
 FOREIGN KEY(branch_id,project_id) REFERENCES branches(id,project_id),
 FOREIGN KEY(parent_handle_id,project_id,branch_id) REFERENCES ai_provider_handles(id,project_id,branch_id),
 FOREIGN KEY(previous_handle_id,project_id,branch_id) REFERENCES ai_provider_handles(id,project_id,branch_id),
 CHECK((kind='response' AND id ~ '^nrp_[a-f0-9]{24}$') OR (kind='conversation' AND id ~ '^ncv_[a-f0-9]{24}$')),
 CHECK(parent_handle_id IS NULL OR previous_handle_id IS NULL),
 CHECK(parent_handle_id IS NULL OR parent_handle_id ~ '^ncv_[a-f0-9]{24}$'),
 CHECK(previous_handle_id IS NULL OR previous_handle_id ~ '^nrp_[a-f0-9]{24}$'),
 CHECK(kind='response' OR (parent_handle_id IS NULL AND previous_handle_id IS NULL)),
 CHECK(expires_at>created_at AND expires_at<=created_at+interval '30 days'),
 CHECK(date_trunc('second',expires_at)=expires_at),
 CHECK(state NOT IN ('ready','deleting') OR (ciphertext IS NOT NULL AND remote_identity_sha256 IS NOT NULL)),
 CHECK((ciphertext IS NULL AND remote_identity_sha256 IS NULL) OR remote_identity_sha256 IS NOT NULL),
 CHECK(state<>'deleted' OR ciphertext IS NULL)
);
-- A supplier resource may never be adopted by a second platform handle, even
-- after logical deletion or under a different branch or encryption key version.
CREATE UNIQUE INDEX ai_provider_handles_remote_identity ON ai_provider_handles(provider_identity,remote_identity_sha256) WHERE remote_identity_sha256 IS NOT NULL;
CREATE INDEX ai_provider_handles_scope ON ai_provider_handles(branch_id,created_at,id);
CREATE INDEX ai_provider_handles_expiry ON ai_provider_handles(expires_at,id) WHERE state NOT IN ('deleted','revoked');

CREATE TABLE ai_provider_state_uses (
 request_id text NOT NULL,
 handle_id text NOT NULL,
 project_id text NOT NULL,
 branch_id text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(request_id,handle_id),
 FOREIGN KEY(request_id,project_id,branch_id) REFERENCES ai_gateway_requests(id,project_id,branch_id),
 FOREIGN KEY(handle_id,project_id,branch_id) REFERENCES ai_provider_handles(id,project_id,branch_id)
);

CREATE TABLE ai_provider_state_mutations (
 id text PRIMARY KEY CHECK(id ~ '^aps_[a-f0-9]{24}$'),
 actor_id text NOT NULL REFERENCES users(id),
 project_id text NOT NULL,
 org_id text NOT NULL,
 branch_id text NOT NULL,
 handle_id text NOT NULL,
 action text NOT NULL CHECK(action IN ('create','update','delete','create_items','delete_item')),
 key_hash text NOT NULL,
 key_version text NOT NULL,
 request_hash text NOT NULL,
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','succeeded','uncertain','failed','reconciled')),
 response_ciphertext bytea CHECK(octet_length(response_ciphertext) BETWEEN 28 AND 65564),
 http_status integer CHECK(http_status BETWEEN 200 AND 599),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 finished_at timestamptz,
 UNIQUE(actor_id,key_hash),
 FOREIGN KEY(project_id,org_id) REFERENCES projects(id,org_id),
 FOREIGN KEY(branch_id,project_id) REFERENCES branches(id,project_id),
 FOREIGN KEY(handle_id,project_id,branch_id) REFERENCES ai_provider_handles(id,project_id,branch_id),
 CHECK((state IN ('succeeded','failed','reconciled'))=(finished_at IS NOT NULL)),
 CHECK(state<>'succeeded' OR (response_ciphertext IS NOT NULL AND http_status IN (200,201)))
);

CREATE FUNCTION guard_ai_provider_handle() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'Provider handle history is retained'; END IF;
 IF (NEW.id,NEW.kind,NEW.project_id,NEW.org_id,NEW.branch_id,NEW.actor_id,NEW.model_id,NEW.provider_id,NEW.provider_revision_id,NEW.provider_identity,NEW.key_version,NEW.request_id,NEW.parent_handle_id,NEW.previous_handle_id,NEW.created_at,NEW.expires_at)
 IS DISTINCT FROM (OLD.id,OLD.kind,OLD.project_id,OLD.org_id,OLD.branch_id,OLD.actor_id,OLD.model_id,OLD.provider_id,OLD.provider_revision_id,OLD.provider_identity,OLD.key_version,OLD.request_id,OLD.parent_handle_id,OLD.previous_handle_id,OLD.created_at,OLD.expires_at)
 OR OLD.remote_identity_sha256 IS NOT NULL AND NEW.remote_identity_sha256 IS DISTINCT FROM OLD.remote_identity_sha256
 OR OLD.ciphertext IS NOT NULL AND NEW.ciphertext IS DISTINCT FROM OLD.ciphertext AND NOT (NEW.state='deleted' AND NEW.ciphertext IS NULL)
 OR OLD.state='deleted' AND NEW IS DISTINCT FROM OLD THEN
  RAISE EXCEPTION 'Provider handle scope, identity, content and lifetime are immutable';
 END IF;
 IF NEW.state IS DISTINCT FROM OLD.state AND NOT (
  OLD.state='pending' AND NEW.state IN ('ready','uncertain','deleted','revoked') OR
  OLD.state='ready' AND NEW.state IN ('uncertain','deleting','revoked') OR
  OLD.state='uncertain' AND NEW.state IN ('ready','deleting','deleted','revoked') OR
  OLD.state='deleting' AND NEW.state IN ('deleted','uncertain','revoked') OR
  OLD.state='revoked' AND NEW.state='deleting'
 ) THEN RAISE EXCEPTION 'Provider handle transition rejected'; END IF;
 IF NEW.version<>OLD.version+1 OR NEW.updated_at<OLD.updated_at THEN
  RAISE EXCEPTION 'Provider handle version must advance once';
 END IF;
 IF OLD.provider_status IN ('completed','incomplete','failed','cancelled','deleted') AND NEW.provider_status IS DISTINCT FROM OLD.provider_status AND NEW.provider_status<>'deleted' THEN
  RAISE EXCEPTION 'Terminal supplier status cannot regress';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER ai_provider_handle_guard BEFORE UPDATE OR DELETE ON ai_provider_handles FOR EACH ROW EXECUTE FUNCTION guard_ai_provider_handle();

CREATE FUNCTION guard_ai_provider_state_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'Provider mutation history is retained'; END IF;
 IF (NEW.id,NEW.actor_id,NEW.project_id,NEW.org_id,NEW.branch_id,NEW.handle_id,NEW.action,NEW.key_hash,NEW.key_version,NEW.request_hash,NEW.created_at)
 IS DISTINCT FROM (OLD.id,OLD.actor_id,OLD.project_id,OLD.org_id,OLD.branch_id,OLD.handle_id,OLD.action,OLD.key_hash,OLD.key_version,OLD.request_hash,OLD.created_at)
 OR OLD.state IN ('succeeded','failed','reconciled') AND NEW IS DISTINCT FROM OLD
 OR NEW.state NOT IN ('pending','succeeded','uncertain','failed','reconciled')
 OR OLD.state='uncertain' AND NEW.state='pending' THEN
  RAISE EXCEPTION 'Provider mutation receipt is immutable';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER ai_provider_state_mutation_guard BEFORE UPDATE OR DELETE ON ai_provider_state_mutations FOR EACH ROW EXECUTE FUNCTION guard_ai_provider_state_mutation();
CREATE TRIGGER ai_provider_state_use_guard BEFORE UPDATE OR DELETE ON ai_provider_state_uses FOR EACH ROW EXECUTE FUNCTION guard_ai_input_receipt();

CREATE TABLE ai_provider_state_reconciliations (
 id text PRIMARY KEY CHECK(id ~ '^apr_[a-f0-9]{24}$'),
 handle_id text NOT NULL,
 mutation_id text UNIQUE REFERENCES ai_provider_state_mutations(id),
 actor_id text NOT NULL REFERENCES users(id),
 project_id text NOT NULL,
 branch_id text NOT NULL,
 key_hash text NOT NULL,
 key_version text NOT NULL,
 request_hash text NOT NULL,
 resolution text NOT NULL CHECK(resolution IN ('adopted','confirmed_applied','confirmed_not_accepted','deleted')),
 evidence_sha256 text NOT NULL CHECK(evidence_sha256 ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(actor_id,key_hash),
 FOREIGN KEY(handle_id,project_id,branch_id) REFERENCES ai_provider_handles(id,project_id,branch_id)
);
CREATE TRIGGER ai_provider_state_reconciliation_guard BEFORE UPDATE OR DELETE ON ai_provider_state_reconciliations FOR EACH ROW EXECUTE FUNCTION guard_ai_input_receipt();

-- Logical branch/project removal withdraws references. Recovery never clones
-- them into another branch or silently makes revoked supplier state reusable.
CREATE FUNCTION revoke_ai_provider_branch_state() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.state IN ('deleting','deleted','recovering') OR NEW.deleted_at IS NOT NULL THEN
  IF TG_TABLE_NAME='branches' THEN
   UPDATE ai_provider_handles SET state='revoked',version=version+1,updated_at=clock_timestamp() WHERE branch_id=NEW.id AND state NOT IN ('deleted','revoked');
  ELSE
   UPDATE ai_provider_handles SET state='revoked',version=version+1,updated_at=clock_timestamp() WHERE project_id=NEW.id AND state NOT IN ('deleted','revoked');
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER ai_provider_branch_lifecycle AFTER UPDATE OF state,deleted_at ON branches FOR EACH ROW EXECUTE FUNCTION revoke_ai_provider_branch_state();
CREATE TRIGGER ai_provider_project_lifecycle AFTER UPDATE OF state,deleted_at ON projects FOR EACH ROW EXECUTE FUNCTION revoke_ai_provider_branch_state();
