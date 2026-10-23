-- Run as the metadata owner after migration 064. Do not grant raw provider or
-- finance policy access to the independent inference process.
BEGIN;
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='neon_ai_gateway' AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolbypassrls) THEN
  RAISE EXCEPTION 'An existing restricted neon_ai_gateway role is required';
 END IF;
END $$;
GRANT SELECT ON ai_input_file_scopes,ai_input_files,ai_input_mutations,ai_request_input_files TO neon_ai_gateway;
GRANT INSERT ON ai_input_files,ai_input_mutations,ai_request_input_files TO neon_ai_gateway;
GRANT UPDATE(deleted_at,ciphertext) ON ai_input_files TO neon_ai_gateway;
COMMIT;
