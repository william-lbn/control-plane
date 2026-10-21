-- Functional TTL retires access/compute only. All native business data is held.
ALTER TABLE branches ADD COLUMN expires_at timestamptz;
ALTER TABLE branches ADD COLUMN ttl_interval_seconds integer;
ALTER TABLE branches ADD COLUMN expiration_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE branches ADD COLUMN expiration_actor_id text;
ALTER TABLE branches ADD COLUMN expiration_operation_id text REFERENCES operations(id);
ALTER TABLE branches ADD COLUMN expiration_status text NOT NULL DEFAULT 'none'
 CHECK(expiration_status IN ('none','scheduled','blocked','retiring','retired'));
ALTER TABLE branches ADD COLUMN expiration_error_code text;
ALTER TABLE branches ADD COLUMN expiration_last_checked_at timestamptz;
ALTER TABLE branches ADD CONSTRAINT expiration_leaf_protection
 CHECK(expires_at IS NULL OR (NOT protected AND NOT is_default AND ttl_interval_seconds BETWEEN 1 AND 2592000));
CREATE INDEX branch_expiration_due ON branches(expires_at,id) WHERE expires_at IS NOT NULL AND deleted_at IS NULL;

CREATE FUNCTION guard_expiring_parent() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE deadline timestamptz;
BEGIN
 IF NEW.parent_branch_id IS NOT NULL THEN
   SELECT expires_at INTO deadline FROM branches WHERE id=NEW.parent_branch_id AND project_id=NEW.project_id FOR UPDATE;
   IF deadline IS NOT NULL THEN RAISE EXCEPTION 'expiring branches cannot acquire children' USING ERRCODE='23514'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER branch_expiring_parent BEFORE INSERT ON branches FOR EACH ROW EXECUTE FUNCTION guard_expiring_parent();

CREATE FUNCTION guard_branch_expiration_operation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE bid text; deadline timestamptz; status text; generation bigint; deleting text;
BEGIN
 IF NEW.action='expire_branch' THEN
   IF NEW.resource_type IS DISTINCT FROM 'branch_lifecycle' OR jsonb_array_length(NEW.payload->'branch_ids')<>1 THEN
     RAISE EXCEPTION 'exact branch expiration scope required' USING ERRCODE='23514';
   END IF;
   bid:=NEW.payload->'branch_ids'->>0;
   PERFORM id FROM projects WHERE id=NEW.project_id AND state='ready' AND deleted_at IS NULL FOR UPDATE;
   IF NOT FOUND THEN RAISE EXCEPTION 'ready expiration parent required' USING ERRCODE='23514'; END IF;
   SELECT expires_at,state,expiration_generation,deletion_operation_id INTO deadline,status,generation,deleting
    FROM branches WHERE id=bid AND project_id=NEW.project_id AND NOT protected AND NOT is_default FOR UPDATE;
   IF deadline IS NULL OR deadline>clock_timestamp() OR NEW.resource_id IS DISTINCT FROM bid OR NEW.payload->>'project_id' IS DISTINCT FROM NEW.project_id
     OR generation::text IS DISTINCT FROM NEW.payload->>'expiration_generation'
     OR (TG_OP='INSERT' AND status IS DISTINCT FROM 'ready')
     OR (TG_OP='UPDATE' AND (deleting IS DISTINCT FROM NEW.id OR status NOT IN ('deleting','deleted'))) THEN
     RAISE EXCEPTION 'original due expiration generation required' USING ERRCODE='23514';
   END IF;
   RETURN NEW;
 END IF;
 IF NEW.action IN ('delete_project','delete_branch','delete_endpoint','cleanup_failed_project','cleanup_failed_branch','cleanup_failed_endpoint','recover_project','set_branch_expiration') THEN RETURN NEW; END IF;
 bid:=NULLIF(NEW.payload->>'branch_id','');
 IF NEW.resource_type='endpoint' THEN SELECT branch_id INTO bid FROM endpoints WHERE id=NEW.resource_id AND project_id=NEW.project_id; END IF;
 IF bid IS NULL THEN RETURN NEW; END IF;
 SELECT expires_at INTO deadline FROM branches WHERE id=bid AND project_id=NEW.project_id FOR UPDATE;
 IF deadline IS NOT NULL AND (deadline<=clock_timestamp() OR NEW.action='create_endpoint' OR NEW.action LIKE '%function%' OR NEW.action='enable_data_api') THEN
   RAISE EXCEPTION 'branch expiration does not admit this work' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER scope_branch_expiration_admission BEFORE INSERT ON operations FOR EACH ROW EXECUTE FUNCTION guard_branch_expiration_operation();
CREATE TRIGGER scope_branch_expiration_retry BEFORE UPDATE OF state ON operations
 FOR EACH ROW WHEN(NEW.state='queued' AND OLD.state='failed') EXECUTE FUNCTION guard_branch_expiration_operation();
