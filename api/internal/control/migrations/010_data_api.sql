-- Public identity-provider keys are configuration, never application secrets.
-- Runtime credentials live only in namespace-scoped immutable Kubernetes Secrets.
CREATE TABLE data_api_instances (
    branch_id text PRIMARY KEY REFERENCES branches(id),
    project_id text NOT NULL REFERENCES projects(id),
    endpoint_id text NOT NULL REFERENCES endpoints(id),
    generation bigint NOT NULL CHECK (generation > 0),
    state text NOT NULL CHECK (state IN ('provisioning','active','disabling','disabled','degraded')),
    spec jsonb NOT NULL,
    secret_ref text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
-- Retain audience ownership after disable: a still-valid parent token must not
-- become a credential for a subsequently enabled child or another tenant.
CREATE UNIQUE INDEX data_api_provider_audience ON data_api_instances ((spec->>'issuer'),(spec->>'audience'));
CREATE UNIQUE INDEX one_active_data_api_operation ON operations(resource_id)
    WHERE resource_type='data_api' AND state IN ('queued','running','retry_wait');
