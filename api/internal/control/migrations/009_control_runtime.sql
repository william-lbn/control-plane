-- One cooperating controller leader per metadata database. Epoch is durable
-- for observability and takeover tests; external actions still require fencing.
CREATE TABLE control_runtime_leases (
    name text PRIMARY KEY CHECK (name = 'controllers'),
    owner_id text NOT NULL,
    epoch bigint NOT NULL CHECK (epoch > 0),
    process_role text NOT NULL CHECK (process_role IN ('all', 'worker')),
    lease_expires_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
