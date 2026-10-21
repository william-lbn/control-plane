-- Customer connection defaults are independent of Compute bootstrap/service
-- identities. Never replace endpoint credentials or rename a platform role.
CREATE TABLE branch_connection_defaults (
 branch_id text PRIMARY KEY REFERENCES branches(id),
 mode text NOT NULL CHECK(mode IN ('platform','managed')),
 role_name text,
 database_name text,
 role_oid bigint CHECK(role_oid BETWEEN 1 AND 4294967295),
 database_oid bigint CHECK(database_oid BETWEEN 1 AND 4294967295),
 tenant_id text,
 timeline_id text,
 version bigint NOT NULL CHECK(version>=2),
 updated_by text NOT NULL REFERENCES users(id),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((mode='managed' AND role_name IS NOT NULL AND database_name IS NOT NULL
        AND role_oid IS NOT NULL AND database_oid IS NOT NULL
        AND tenant_id IS NOT NULL AND timeline_id IS NOT NULL
        AND tenant_id ~ '^[a-f0-9]{32}$' AND timeline_id ~ '^[a-f0-9]{32}$')
    OR (mode='platform' AND role_name IS NULL AND database_name IS NULL
        AND role_oid IS NULL AND database_oid IS NULL AND tenant_id IS NULL AND timeline_id IS NULL))
);
CREATE TABLE connection_default_requests (
 actor_id text NOT NULL REFERENCES users(id),
 key_hash text NOT NULL,
 request_hash text NOT NULL,
 branch_id text NOT NULL REFERENCES branches(id),
 response jsonb NOT NULL CHECK(jsonb_typeof(response)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(actor_id,key_hash)
);
