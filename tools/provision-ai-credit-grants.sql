-- Metadata owner, after migration 062 and before switching the AI Gateway.
-- Grants allow admission/settlement, never grants of credit or policy changes.
BEGIN;
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='neon_ai_gateway' AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolbypassrls) THEN
  RAISE EXCEPTION 'An existing restricted neon_ai_gateway role is required';
 END IF;
END $$;
GRANT SELECT(id,project_id) ON branches TO neon_ai_gateway;
GRANT SELECT(id,org_id) ON projects TO neon_ai_gateway;
GRANT SELECT ON ai_credit_accounts,ai_credit_reservations TO neon_ai_gateway;
GRANT UPDATE(spent_micros,reserved_micros) ON ai_credit_accounts TO neon_ai_gateway;
GRANT INSERT ON ai_credit_reservations TO neon_ai_gateway;
GRANT UPDATE(state,actual_micros,usage,settled_at) ON ai_credit_reservations TO neon_ai_gateway;
GRANT INSERT(id,org_id,request_id,kind,spent_delta,reserved_delta,actor_id,evidence_sha256) ON ai_credit_ledger TO neon_ai_gateway;
COMMIT;
