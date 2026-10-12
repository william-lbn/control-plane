-- Forward-only retained deployment lineage. Rollback creates a new deployment,
-- generation and boot; it never rewinds database data or resurrects an old VM.
ALTER TABLE function_deployments ADD COLUMN source_deployment_id text;
ALTER TABLE function_deployments ADD CONSTRAINT function_deployment_source_scope
 FOREIGN KEY(source_deployment_id,function_id,project_id,branch_id)
 REFERENCES function_deployments(id,function_id,project_id,branch_id);
ALTER TABLE function_deployments ADD CONSTRAINT function_deployment_source_not_self
 CHECK(source_deployment_id IS NULL OR source_deployment_id<>id);

CREATE FUNCTION guard_function_deployment_lineage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND NEW.source_deployment_id IS DISTINCT FROM OLD.source_deployment_id THEN
  RAISE EXCEPTION 'function deployment lineage is immutable' USING ERRCODE='23514';
 END IF;
 IF TG_OP='INSERT' AND NEW.source_deployment_id IS NOT NULL AND NOT EXISTS(
  SELECT 1 FROM function_deployments d WHERE d.id=NEW.source_deployment_id
  AND d.function_id=NEW.function_id AND d.project_id=NEW.project_id
  AND d.branch_id=NEW.branch_id AND d.state='completed') THEN
  RAISE EXCEPTION 'function source must be an owned completed deployment' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER function_deployment_lineage_guard BEFORE INSERT OR UPDATE ON function_deployments
 FOR EACH ROW EXECUTE FUNCTION guard_function_deployment_lineage();
