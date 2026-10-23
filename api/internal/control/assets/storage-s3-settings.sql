-- These settings live with the branch directory and follow native timeline
-- cloning. No object byte reference, previous manifest or WAL is removed.
ALTER TABLE neon_storage.buckets ADD COLUMN IF NOT EXISTS settings jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(settings)='object');
ALTER TABLE neon_storage.buckets ADD COLUMN IF NOT EXISTS settings_version bigint NOT NULL DEFAULT 1 CHECK(settings_version>0);
ALTER TABLE neon_storage.objects ADD COLUMN IF NOT EXISTS tags jsonb NOT NULL DEFAULT '[]'::jsonb CHECK(jsonb_typeof(tags)='array');
ALTER TABLE neon_storage.objects ADD COLUMN IF NOT EXISTS response_headers jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(response_headers)='object');
ALTER TABLE neon_storage.multipart_uploads ADD COLUMN IF NOT EXISTS tags jsonb NOT NULL DEFAULT '[]'::jsonb CHECK(jsonb_typeof(tags)='array');
ALTER TABLE neon_storage.multipart_uploads ADD COLUMN IF NOT EXISTS response_headers jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(response_headers)='object');
CREATE TABLE IF NOT EXISTS neon_storage.setting_mutations (
 branch_id text NOT NULL,
 actor_id text NOT NULL,
 key_hash text NOT NULL,
 request_hash text NOT NULL,
 response jsonb NOT NULL,
 etag text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(branch_id,actor_id,key_hash)
);
CREATE OR REPLACE FUNCTION neon_storage.guard_setting_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'Storage setting mutation receipts are immutable'; END $$;
DROP TRIGGER IF EXISTS immutable_setting_mutation ON neon_storage.setting_mutations;
CREATE TRIGGER immutable_setting_mutation BEFORE UPDATE OR DELETE ON neon_storage.setting_mutations FOR EACH ROW EXECUTE FUNCTION neon_storage.guard_setting_mutation();
