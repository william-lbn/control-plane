-- Run as metadata owner after migration 066. The gateway cannot retag owners,
-- delete history, change finance policy or replace an established remote ID.
BEGIN;
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='neon_ai_gateway' AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolbypassrls) THEN
  RAISE EXCEPTION 'An existing restricted neon_ai_gateway role is required';
 END IF;
END $$;
GRANT SELECT ON ai_provider_handles,ai_provider_state_uses,ai_provider_state_mutations TO neon_ai_gateway;
GRANT INSERT ON ai_provider_handles,ai_provider_state_uses,ai_provider_state_mutations TO neon_ai_gateway;
GRANT SELECT,INSERT ON ai_provider_state_reconciliations TO neon_ai_gateway;
GRANT UPDATE(state,provider_status,ciphertext,remote_identity_sha256,updated_at,version) ON ai_provider_handles TO neon_ai_gateway;
GRANT UPDATE(state,response_ciphertext,http_status,finished_at) ON ai_provider_state_mutations TO neon_ai_gateway;
COMMIT;
