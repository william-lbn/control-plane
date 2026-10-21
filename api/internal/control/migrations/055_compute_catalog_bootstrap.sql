-- A first SQL bootstrap and a later observation of an already bootstrapped
-- Endpoint are distinct witnesses. Neither invents pre-observation SQL history.
CREATE TABLE compute_catalog_bootstrap_receipts (
 endpoint_id text PRIMARY KEY REFERENCES endpoints(id),
 operation_id text NOT NULL REFERENCES operations(id),
 proof jsonb NOT NULL CHECK(jsonb_typeof(proof)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE compute_catalog_bootstrap_publications (
 endpoint_id text PRIMARY KEY REFERENCES compute_catalog_bootstrap_receipts(endpoint_id),
 published_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE FUNCTION preserve_compute_catalog_bootstrap() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'Compute catalog bootstrap witness is immutable' USING ERRCODE='23514';
END $$;
CREATE TRIGGER preserve_compute_catalog_bootstrap_receipt BEFORE UPDATE OR DELETE ON compute_catalog_bootstrap_receipts FOR EACH ROW EXECUTE FUNCTION preserve_compute_catalog_bootstrap();
CREATE TRIGGER preserve_compute_catalog_bootstrap_publication BEFORE UPDATE OR DELETE ON compute_catalog_bootstrap_publications FOR EACH ROW EXECUTE FUNCTION preserve_compute_catalog_bootstrap();
