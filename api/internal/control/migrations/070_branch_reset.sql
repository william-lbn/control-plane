-- Restore epochs retain the original Endpoint bootstrap and native timelines.
-- Private rollback bytes remain in separately owned immutable Secrets; these
-- witnesses contain references, identities and digests only.
CREATE TABLE branch_reset_checkpoints (
 operation_id text PRIMARY KEY REFERENCES operations(id),
 project_id text NOT NULL REFERENCES projects(id),
 branch_id text NOT NULL REFERENCES branches(id),
 source_branch_id text NOT NULL REFERENCES branches(id),
 tenant_id text NOT NULL CHECK(tenant_id ~ '^[a-f0-9]{32}$'),
 original_timeline_id text NOT NULL CHECK(original_timeline_id ~ '^[a-f0-9]{32}$'),
 source_timeline_id text NOT NULL CHECK(source_timeline_id ~ '^[a-f0-9]{32}$'),
 target_timeline_id text NOT NULL UNIQUE CHECK(target_timeline_id ~ '^[a-f0-9]{32}$'),
 backup_branch_id text NOT NULL UNIQUE REFERENCES branches(id),
 backup_timeline_id text NOT NULL UNIQUE CHECK(backup_timeline_id ~ '^[a-f0-9]{32}$'),
 backup_lsn text NOT NULL CHECK(backup_lsn ~ '^[0-9A-F]{1,8}/[0-9A-F]{1,8}$'),
 restore_lsn text NOT NULL CHECK(restore_lsn ~ '^[0-9A-F]{1,8}/[0-9A-F]{1,8}$'),
 original_branch_version bigint NOT NULL CHECK(original_branch_version>0),
 payload_sha256 text NOT NULL CHECK(payload_sha256 ~ '^[a-f0-9]{64}$'),
 metadata_sha256 text NOT NULL CHECK(metadata_sha256 ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK(backup_branch_id<>branch_id AND backup_branch_id<>source_branch_id),
 CHECK(target_timeline_id<>original_timeline_id AND target_timeline_id<>source_timeline_id
   AND target_timeline_id<>backup_timeline_id AND backup_timeline_id<>original_timeline_id
   AND backup_timeline_id<>source_timeline_id)
);

CREATE TABLE branch_reset_endpoint_backings (
 operation_id text NOT NULL REFERENCES branch_reset_checkpoints(operation_id),
 endpoint_id text NOT NULL REFERENCES endpoints(id),
 endpoint_type text NOT NULL CHECK(endpoint_type IN ('read_write','read_only')),
 original_config_uid text NOT NULL CHECK(length(original_config_uid) BETWEEN 1 AND 128),
 original_credential_uid text NOT NULL CHECK(length(original_credential_uid) BETWEEN 1 AND 128),
 rollback_secret_ref text NOT NULL CHECK(length(rollback_secret_ref) BETWEEN 1 AND 253
   AND rollback_secret_ref ~ '^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$'),
 rollback_secret_uid text NOT NULL CHECK(length(rollback_secret_uid) BETWEEN 1 AND 128),
 rollback_sha256 text NOT NULL CHECK(rollback_sha256 ~ '^[a-f0-9]{64}$'),
 prepared_config_sha256 text NOT NULL CHECK(prepared_config_sha256 ~ '^[a-f0-9]{64}$'),
 original_endpoint_version bigint NOT NULL CHECK(original_endpoint_version>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(operation_id,endpoint_id)
);

CREATE TABLE compute_catalog_reset_receipts (
 operation_id text NOT NULL,
 endpoint_id text NOT NULL,
 proof jsonb NOT NULL CHECK(jsonb_typeof(proof)='object'
   AND proof ?& ARRAY['project_id','branch_id','endpoint_id','endpoint_type','tenant_id',
     'timeline_id','config_uid','credential_uid','operation_id','witness']
   AND (proof-ARRAY['project_id','branch_id','endpoint_id','endpoint_type','tenant_id',
     'timeline_id','config_uid','credential_uid','operation_id','witness'])='{}'::jsonb
   AND proof->>'operation_id'=operation_id AND proof->>'endpoint_id'=endpoint_id
   AND proof->>'witness'='branch_reset_sql_probe_v1'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(operation_id,endpoint_id),
 FOREIGN KEY(operation_id,endpoint_id) REFERENCES branch_reset_endpoint_backings(operation_id,endpoint_id)
);

CREATE TABLE compute_catalog_reset_publications (
 operation_id text NOT NULL,
 endpoint_id text NOT NULL,
 published_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(operation_id,endpoint_id),
 FOREIGN KEY(operation_id,endpoint_id) REFERENCES compute_catalog_reset_receipts(operation_id,endpoint_id)
);

CREATE TABLE branch_reset_results (
 operation_id text PRIMARY KEY REFERENCES branch_reset_checkpoints(operation_id),
 result jsonb NOT NULL CHECK(jsonb_typeof(result)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE FUNCTION preserve_branch_reset_witness() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'Original branch reset witness is immutable' USING ERRCODE='23514';
END $$;
CREATE TRIGGER preserve_branch_reset_checkpoint BEFORE UPDATE OR DELETE ON branch_reset_checkpoints
 FOR EACH ROW EXECUTE FUNCTION preserve_branch_reset_witness();
CREATE TRIGGER preserve_branch_reset_endpoint_backing BEFORE UPDATE OR DELETE ON branch_reset_endpoint_backings
 FOR EACH ROW EXECUTE FUNCTION preserve_branch_reset_witness();
CREATE TRIGGER preserve_compute_catalog_reset_receipt BEFORE UPDATE OR DELETE ON compute_catalog_reset_receipts
 FOR EACH ROW EXECUTE FUNCTION preserve_branch_reset_witness();
CREATE TRIGGER preserve_compute_catalog_reset_publication BEFORE UPDATE OR DELETE ON compute_catalog_reset_publications
 FOR EACH ROW EXECUTE FUNCTION preserve_branch_reset_witness();
CREATE TRIGGER preserve_branch_reset_result BEFORE UPDATE OR DELETE ON branch_reset_results
 FOR EACH ROW EXECUTE FUNCTION preserve_branch_reset_witness();

CREATE FUNCTION check_branch_reset_bootstrap_scope() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(
  SELECT 1 FROM branch_reset_checkpoints c
  JOIN branch_reset_endpoint_backings k ON k.operation_id=c.operation_id
    AND k.endpoint_id=NEW.endpoint_id
  JOIN endpoints e ON e.id=k.endpoint_id AND e.project_id=c.project_id AND e.branch_id=c.branch_id
  JOIN branches b ON b.id=c.branch_id AND b.project_id=c.project_id
  JOIN projects p ON p.id=c.project_id AND p.tenant_id=c.tenant_id
  WHERE c.operation_id=NEW.operation_id
    AND NEW.proof->>'project_id'=c.project_id AND NEW.proof->>'branch_id'=c.branch_id
    AND NEW.proof->>'tenant_id'=c.tenant_id AND NEW.proof->>'timeline_id'=c.target_timeline_id
    AND NEW.proof->>'endpoint_type'=k.endpoint_type
    AND NEW.proof->>'config_uid'=k.original_config_uid
    AND NEW.proof->>'credential_uid'=k.original_credential_uid
 ) THEN
  RAISE EXCEPTION 'Restored Compute bootstrap must retain its exact original backing scope' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER check_branch_reset_bootstrap_scope BEFORE INSERT ON compute_catalog_reset_receipts
 FOR EACH ROW EXECUTE FUNCTION check_branch_reset_bootstrap_scope();

-- Publication becomes visible together with the branch's metadata cutover.
-- Deferred checking permits either insertion order in that one transaction.
CREATE FUNCTION check_branch_reset_publication_cutover() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(
  SELECT 1 FROM compute_catalog_reset_receipts r
  JOIN branch_reset_checkpoints c ON c.operation_id=r.operation_id
  JOIN branches b ON b.id=c.branch_id AND b.project_id=c.project_id
    AND b.timeline_id=c.target_timeline_id AND b.state='ready' AND b.deleted_at IS NULL
  JOIN endpoints e ON e.id=r.endpoint_id AND e.project_id=c.project_id AND e.branch_id=b.id
    AND e.state='suspended' AND e.deleted_at IS NULL
  WHERE r.operation_id=NEW.operation_id AND r.endpoint_id=NEW.endpoint_id
 ) THEN
  RAISE EXCEPTION 'Restored Compute publication requires the same atomic branch cutover' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE CONSTRAINT TRIGGER check_branch_reset_publication_cutover
 AFTER INSERT ON compute_catalog_reset_publications DEFERRABLE INITIALLY DEFERRED
 FOR EACH ROW EXECUTE FUNCTION check_branch_reset_publication_cutover();
