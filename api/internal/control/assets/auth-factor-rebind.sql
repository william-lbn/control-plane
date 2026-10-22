-- The receipt and rewrapped factors commit with the first target installation.
-- Runtime identities receive no privileges on this control-only receipt.
CREATE TABLE IF NOT EXISTS neon_auth.control_factor_rebindings (
 branch_id text PRIMARY KEY,
 proof jsonb NOT NULL CHECK(jsonb_typeof(proof)='object'),
 mac text NOT NULL CHECK(mac ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE OR REPLACE FUNCTION neon_auth.preserve_factor_rebinding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'Auth factor rebinding receipt is immutable' USING ERRCODE='23514';
END $$;
DROP TRIGGER IF EXISTS preserve_factor_rebinding ON neon_auth.control_factor_rebindings;
CREATE TRIGGER preserve_factor_rebinding BEFORE UPDATE OR DELETE ON neon_auth.control_factor_rebindings FOR EACH ROW EXECUTE FUNCTION neon_auth.preserve_factor_rebinding();
