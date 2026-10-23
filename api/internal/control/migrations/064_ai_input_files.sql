-- Gateway-owned input references never expose supplier account file handles.
CREATE TABLE ai_input_files (
 id text PRIMARY KEY CHECK(id ~ '^nif_[a-f0-9]{24}$'),
 project_id text NOT NULL,
 org_id text NOT NULL,
 branch_id text NOT NULL,
 actor_id text NOT NULL REFERENCES users(id),
 filename text NOT NULL CHECK(length(filename) BETWEEN 1 AND 128),
 content_type text NOT NULL CHECK(content_type IN ('image/png','image/jpeg','image/gif','image/webp','application/pdf','audio/wav','audio/mpeg','video/mp4')),
 purpose text NOT NULL CHECK(purpose IN ('user_data','vision')),
 bytes integer NOT NULL CHECK(bytes BETWEEN 1 AND 1048576),
 sha256 text NOT NULL CHECK(sha256 ~ '^[a-f0-9]{64}$'),
 key_version text NOT NULL,
 ciphertext bytea,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL,
 deleted_at timestamptz,
 UNIQUE(id,project_id,branch_id),
 FOREIGN KEY(project_id,org_id) REFERENCES projects(id,org_id),
 FOREIGN KEY(branch_id,project_id) REFERENCES branches(id,project_id),
 CHECK(expires_at>created_at AND expires_at<=created_at+interval '7 days'),
 CHECK((deleted_at IS NULL)=(ciphertext IS NOT NULL)),
 CHECK(ciphertext IS NULL OR octet_length(ciphertext)=bytes+28),
 CHECK(purpose<>'vision' OR content_type LIKE 'image/%')
);
CREATE INDEX ai_input_files_scope ON ai_input_files(branch_id,created_at,id);
CREATE INDEX ai_input_files_expiry ON ai_input_files(expires_at,id) WHERE deleted_at IS NULL;
CREATE TABLE ai_input_mutations (
 actor_id text NOT NULL REFERENCES users(id),
 key_hash text NOT NULL,
 key_version text NOT NULL,
 request_hash text NOT NULL,
 response jsonb NOT NULL,
 status integer NOT NULL CHECK(status IN (200,201)),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(actor_id,key_hash)
);
CREATE TABLE ai_request_input_files (
 request_id text NOT NULL,
 file_id text NOT NULL,
 project_id text NOT NULL,
 branch_id text NOT NULL,
 PRIMARY KEY(request_id,file_id),
 FOREIGN KEY(request_id,project_id,branch_id) REFERENCES ai_gateway_requests(id,project_id,branch_id),
 FOREIGN KEY(file_id,project_id,branch_id) REFERENCES ai_input_files(id,project_id,branch_id)
);
CREATE FUNCTION guard_ai_input_file() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'AI input metadata history is retained'; END IF;
 IF (NEW.id,NEW.project_id,NEW.org_id,NEW.branch_id,NEW.actor_id,NEW.filename,NEW.content_type,NEW.purpose,NEW.bytes,NEW.sha256,NEW.key_version,NEW.created_at,NEW.expires_at)
 IS DISTINCT FROM (OLD.id,OLD.project_id,OLD.org_id,OLD.branch_id,OLD.actor_id,OLD.filename,OLD.content_type,OLD.purpose,OLD.bytes,OLD.sha256,OLD.key_version,OLD.created_at,OLD.expires_at)
 OR OLD.deleted_at IS NOT NULL AND NEW IS DISTINCT FROM OLD
 OR NEW.ciphertext IS DISTINCT FROM OLD.ciphertext AND NEW.ciphertext IS NOT NULL THEN
  RAISE EXCEPTION 'AI input identity, content and expiry are immutable';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER ai_input_file_guard BEFORE UPDATE OR DELETE ON ai_input_files
 FOR EACH ROW EXECUTE FUNCTION guard_ai_input_file();
CREATE FUNCTION guard_ai_input_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'AI input admission and mutation receipts are immutable';
END $$;
CREATE TRIGGER ai_input_mutation_guard BEFORE UPDATE OR DELETE ON ai_input_mutations
 FOR EACH ROW EXECUTE FUNCTION guard_ai_input_receipt();
CREATE TRIGGER ai_request_input_guard BEFORE UPDATE OR DELETE ON ai_request_input_files
 FOR EACH ROW EXECUTE FUNCTION guard_ai_input_receipt();
CREATE VIEW ai_input_file_scopes AS
 SELECT b.id AS branch_id,p.id AS project_id,p.org_id
 FROM branches b JOIN projects p ON p.id=b.project_id JOIN organizations o ON o.id=p.org_id
 WHERE b.state='ready' AND b.deleted_at IS NULL AND p.state='ready' AND p.deleted_at IS NULL AND o.state='active';
