CREATE TABLE managed_auth_factor_results (
 operation_id text PRIMARY KEY REFERENCES operations(id),
 project_id text NOT NULL REFERENCES projects(id),
 branch_id text NOT NULL REFERENCES branches(id),
 source_branch_id text NOT NULL REFERENCES branches(id),
 proof jsonb NOT NULL CHECK(jsonb_typeof(proof)='object'),
 mac text NOT NULL CHECK(mac ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE FUNCTION preserve_managed_auth_factor_result() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'Auth factor migration result is immutable' USING ERRCODE='23514';
END $$;
CREATE TRIGGER preserve_managed_auth_factor_result BEFORE UPDATE OR DELETE ON managed_auth_factor_results FOR EACH ROW EXECUTE FUNCTION preserve_managed_auth_factor_result();
