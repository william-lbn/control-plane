-- Run as the metadata owner after migration 060, before upgrading the Gateway.
-- No passwords, role creation, broad metadata grants or catalog mutation.
BEGIN;
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='neon_ai_gateway' AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolbypassrls) THEN
  RAISE EXCEPTION 'An existing restricted neon_ai_gateway role is required';
 END IF;
END $$;
GRANT SELECT ON ai_budget_policies,ai_budget_reservations TO neon_ai_gateway;
GRANT UPDATE(spent_micros,reserved_micros) ON ai_budget_policies TO neon_ai_gateway;
GRANT INSERT ON ai_budget_reservations,ai_budget_ledger TO neon_ai_gateway;
GRANT UPDATE(state,actual_micros,usage,settled_at) ON ai_budget_reservations TO neon_ai_gateway;
COMMIT;
