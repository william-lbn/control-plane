CREATE TABLE IF NOT EXISTS schema_migrations (
    version integer PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS users (
    id text PRIMARY KEY,
    username text NOT NULL UNIQUE,
    password_hash text NOT NULL,
    role text NOT NULL CHECK (role IN ('owner','admin','developer','viewer')),
    disabled boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash text PRIMARY KEY,
    user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    csrf_hash text NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS sessions_expires_at ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS projects (
    id text PRIMARY KEY,
    org_id text NOT NULL,
    name text NOT NULL,
    tenant_id text UNIQUE,
    region_id text NOT NULL,
    postgres_version integer NOT NULL,
    state text NOT NULL,
    source text NOT NULL,
    default_branch_id text,
    protected boolean NOT NULL DEFAULT true,
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    deleted_at timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS live_project_name ON projects(org_id, lower(name)) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS branches (
    id text PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects(id),
    name text NOT NULL,
    timeline_id text UNIQUE,
    parent_branch_id text REFERENCES branches(id),
    parent_lsn text,
    is_default boolean NOT NULL DEFAULT false,
    protected boolean NOT NULL DEFAULT false,
    state text NOT NULL,
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL,
    deleted_at timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS live_branch_name ON branches(project_id, lower(name)) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS endpoints (
    id text PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects(id),
    branch_id text NOT NULL REFERENCES branches(id),
    selector text NOT NULL UNIQUE,
    workload_kind text NOT NULL,
    workload_name text NOT NULL,
    service_name text NOT NULL,
    state text NOT NULL,
    role_name text NOT NULL,
    database_name text NOT NULL,
    min_cpu_milli integer,
    max_cpu_milli integer,
    min_memory_mib integer,
    max_memory_mib integer,
    slot_size_mib integer,
    scale_to_zero boolean NOT NULL DEFAULT false,
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    deleted_at timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS one_writer_per_branch ON endpoints(branch_id) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS operations (
    id text PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects(id),
    resource_type text NOT NULL,
    resource_id text NOT NULL,
    action text NOT NULL,
    state text NOT NULL CHECK (state IN ('queued','running','retry_wait','succeeded','failed','cancelled')),
    actor_id text NOT NULL,
    request_id text NOT NULL,
    payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    error_code text,
    error_message text,
    retryable boolean NOT NULL DEFAULT false,
    attempts integer NOT NULL DEFAULT 0,
    lease_owner text,
    lease_expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS one_active_endpoint_operation
    ON operations(resource_id) WHERE resource_type='endpoint' AND state IN ('queued','running','retry_wait');
CREATE INDEX IF NOT EXISTS operation_queue ON operations(state,created_at);

CREATE TABLE IF NOT EXISTS operation_steps (
    operation_id text NOT NULL REFERENCES operations(id) ON DELETE CASCADE,
    ordinal integer NOT NULL,
    name text NOT NULL,
    state text NOT NULL,
    attempts integer NOT NULL DEFAULT 0,
    detail text,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(operation_id,ordinal)
);

CREATE TABLE IF NOT EXISTS idempotency_keys (
    actor_id text NOT NULL,
    key_hash text NOT NULL,
    request_hash text NOT NULL,
    operation_id text NOT NULL REFERENCES operations(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(actor_id,key_hash)
);

CREATE TABLE IF NOT EXISTS audit_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_id text NOT NULL,
    action text NOT NULL,
    resource_type text NOT NULL,
    resource_id text NOT NULL,
    outcome text NOT NULL,
    request_id text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS metric_samples (
    endpoint_id text NOT NULL REFERENCES endpoints(id),
    sampled_at timestamptz NOT NULL,
    observed_state text NOT NULL,
    cpu_allocated_milli integer,
    cpu_used_milli double precision,
    memory_allocated_mib integer,
    memory_used_mib double precision,
    connections integer,
    active_connections integer,
    idle_connections integer,
    database_size_bytes bigint,
    deadlocks_total bigint,
    rows_inserted_total bigint,
    rows_updated_total bigint,
    rows_deleted_total bigint,
    errors jsonb NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY(endpoint_id,sampled_at)
);
CREATE INDEX IF NOT EXISTS metric_samples_time ON metric_samples(sampled_at);
