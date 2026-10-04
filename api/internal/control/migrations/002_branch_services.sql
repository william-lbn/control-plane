CREATE TABLE IF NOT EXISTS branch_service_instances (
    branch_id text NOT NULL REFERENCES branches(id),
    service_kind text NOT NULL CHECK (service_kind IN ('postgres','auth','object_storage','functions','ai_gateway','data_api')),
    desired_state text NOT NULL CHECK (desired_state IN ('disabled','provisioning','active','suspended','deleting')),
    observed_state text NOT NULL CHECK (observed_state IN ('disabled','provisioning','active','degraded','suspended','unknown')),
    driver_version text,
    public_endpoint text,
    config_ref text,
    version bigint NOT NULL DEFAULT 1,
    last_observed_at timestamptz,
    PRIMARY KEY(branch_id,service_kind)
);
CREATE INDEX IF NOT EXISTS branch_service_kind_state ON branch_service_instances(service_kind,observed_state);
