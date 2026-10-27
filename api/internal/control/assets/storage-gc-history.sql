-- Explicit GC history instrumentation v1 over native Storage format 7.
-- Never execute from native format-7 enable/replay. The installer verifies an
-- exact project/schema identity and installs the immutable authority row LAST.
CREATE TABLE neon_storage.gc_history_installation (
 singleton boolean PRIMARY KEY CHECK(singleton),
 version integer NOT NULL CHECK(version=1),
 base_format integer NOT NULL CHECK(base_format=7),
 project_id text NOT NULL,
 incarnation text NOT NULL CHECK(incarnation ~ '^sge_[a-f0-9]{24}$'),
 schema_sha256 text NOT NULL CHECK(schema_sha256 ~ '^[a-f0-9]{64}$')
);
CREATE TABLE neon_storage.gc_history_epochs (
 blob_key text PRIMARY KEY,
 epoch bigint NOT NULL CHECK(epoch>0)
);
CREATE TABLE neon_storage.gc_history_retirements (
 blob_key text PRIMARY KEY,
 retirement_id text NOT NULL CHECK(retirement_id ~ '^sgh_[a-f0-9]{24}$')
);
REVOKE ALL ON neon_storage.gc_history_installation,neon_storage.gc_history_epochs,neon_storage.gc_history_retirements FROM PUBLIC;

CREATE FUNCTION neon_storage.gc_history_evidence_guard() RETURNS trigger
 LANGUAGE plpgsql SET search_path=pg_catalog,neon_storage,pg_temp AS $$
BEGIN
 IF TG_TABLE_NAME IN ('gc_history_installation','gc_history_retirements') OR TG_OP='DELETE' OR pg_trigger_depth()<2 THEN
  RAISE EXCEPTION 'GC history authority/epochs are immutable outside reference triggers' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER gc_history_installation_guard BEFORE UPDATE OR DELETE ON neon_storage.gc_history_installation
 FOR EACH ROW EXECUTE FUNCTION neon_storage.gc_history_evidence_guard();
CREATE TRIGGER gc_history_epochs_guard BEFORE INSERT OR UPDATE OR DELETE ON neon_storage.gc_history_epochs
 FOR EACH ROW EXECUTE FUNCTION neon_storage.gc_history_evidence_guard();
ALTER TABLE neon_storage.gc_history_installation ENABLE ALWAYS TRIGGER gc_history_installation_guard;
ALTER TABLE neon_storage.gc_history_epochs ENABLE ALWAYS TRIGGER gc_history_epochs_guard;
CREATE TRIGGER gc_history_retirements_guard BEFORE UPDATE OR DELETE ON neon_storage.gc_history_retirements
 FOR EACH ROW EXECUTE FUNCTION neon_storage.gc_history_evidence_guard();
ALTER TABLE neon_storage.gc_history_retirements ENABLE ALWAYS TRIGGER gc_history_retirements_guard;

CREATE FUNCTION neon_storage.gc_history_reference_changed() RETURNS trigger
 LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,neon_storage,pg_temp AS $$
DECLARE previous_key text; next_key text;
BEGIN
 IF TG_OP='UPDATE' AND OLD.blob_key IS NOT DISTINCT FROM NEW.blob_key THEN RETURN NEW; END IF;
 IF TG_OP<>'INSERT' THEN previous_key:=OLD.blob_key; END IF;
 IF TG_OP<>'DELETE' THEN next_key:=NEW.blob_key; END IF;
 IF next_key IS NOT NULL AND EXISTS(SELECT 1 FROM neon_storage.gc_history_retirements WHERE blob_key=next_key) THEN
  RAISE EXCEPTION 'retired physical object cannot acquire a native reference' USING ERRCODE='23514';
 END IF;
 -- Lock epochs in bytewise key order for replacement writes. An overflow
 -- aborts the reference transaction; no counter wraps to an earlier epoch.
 IF previous_key IS NOT NULL AND next_key IS NOT NULL AND previous_key COLLATE "C">next_key COLLATE "C" THEN
  INSERT INTO neon_storage.gc_history_epochs(blob_key,epoch) VALUES(next_key,1)
   ON CONFLICT(blob_key) DO UPDATE SET epoch=neon_storage.gc_history_epochs.epoch+1;
  next_key:=previous_key; previous_key:=NULL;
 END IF;
 IF previous_key IS NOT NULL THEN
  INSERT INTO neon_storage.gc_history_epochs(blob_key,epoch) VALUES(previous_key,1)
   ON CONFLICT(blob_key) DO UPDATE SET epoch=neon_storage.gc_history_epochs.epoch+1;
 END IF;
 IF next_key IS NOT NULL THEN
  INSERT INTO neon_storage.gc_history_epochs(blob_key,epoch) VALUES(next_key,1)
   ON CONFLICT(blob_key) DO UPDATE SET epoch=neon_storage.gc_history_epochs.epoch+1;
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER gc_history_objects_changed AFTER INSERT OR UPDATE OR DELETE ON neon_storage.objects
 FOR EACH ROW EXECUTE FUNCTION neon_storage.gc_history_reference_changed();
CREATE TRIGGER gc_history_parts_changed AFTER INSERT OR UPDATE OR DELETE ON neon_storage.multipart_parts
 FOR EACH ROW EXECUTE FUNCTION neon_storage.gc_history_reference_changed();
ALTER TABLE neon_storage.objects ENABLE ALWAYS TRIGGER gc_history_objects_changed;
ALTER TABLE neon_storage.multipart_parts ENABLE ALWAYS TRIGGER gc_history_parts_changed;

CREATE FUNCTION neon_storage.gc_history_forbid_truncate() RETURNS trigger
 LANGUAGE plpgsql SET search_path=pg_catalog,neon_storage,pg_temp AS $$
BEGIN
 RAISE EXCEPTION 'GC history instrumentation requires row-observed DELETE, never TRUNCATE' USING ERRCODE='23514';
END $$;
CREATE TRIGGER gc_history_objects_truncate BEFORE TRUNCATE ON neon_storage.objects
 FOR EACH STATEMENT EXECUTE FUNCTION neon_storage.gc_history_forbid_truncate();
CREATE TRIGGER gc_history_parts_truncate BEFORE TRUNCATE ON neon_storage.multipart_parts
 FOR EACH STATEMENT EXECUTE FUNCTION neon_storage.gc_history_forbid_truncate();
CREATE TRIGGER gc_history_epochs_truncate BEFORE TRUNCATE ON neon_storage.gc_history_epochs
 FOR EACH STATEMENT EXECUTE FUNCTION neon_storage.gc_history_forbid_truncate();
CREATE TRIGGER gc_history_installation_truncate BEFORE TRUNCATE ON neon_storage.gc_history_installation
 FOR EACH STATEMENT EXECUTE FUNCTION neon_storage.gc_history_forbid_truncate();
CREATE TRIGGER gc_history_retirements_truncate BEFORE TRUNCATE ON neon_storage.gc_history_retirements
 FOR EACH STATEMENT EXECUTE FUNCTION neon_storage.gc_history_forbid_truncate();
ALTER TABLE neon_storage.objects ENABLE ALWAYS TRIGGER gc_history_objects_truncate;
ALTER TABLE neon_storage.multipart_parts ENABLE ALWAYS TRIGGER gc_history_parts_truncate;
ALTER TABLE neon_storage.gc_history_epochs ENABLE ALWAYS TRIGGER gc_history_epochs_truncate;
ALTER TABLE neon_storage.gc_history_installation ENABLE ALWAYS TRIGGER gc_history_installation_truncate;
ALTER TABLE neon_storage.gc_history_retirements ENABLE ALWAYS TRIGGER gc_history_retirements_truncate;
REVOKE ALL ON FUNCTION neon_storage.gc_history_reference_changed(),neon_storage.gc_history_evidence_guard(),neon_storage.gc_history_forbid_truncate() FROM PUBLIC;
