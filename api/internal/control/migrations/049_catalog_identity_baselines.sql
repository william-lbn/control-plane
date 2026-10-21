-- An explicit initial observation, not a claim of identity before this point.
-- Rows remain immutable through ordinary catalog operations. Intent version
-- changes are reported separately; no observation silently adopts a new OID.
CREATE TABLE catalog_identity_baselines (
 id text PRIMARY KEY,
 project_id text NOT NULL REFERENCES projects(id),
 branch_id text NOT NULL REFERENCES branches(id),
 kind text NOT NULL CHECK(kind IN ('role','database')),
 name text NOT NULL,
 object_oid bigint NOT NULL CHECK(object_oid BETWEEN 1 AND 4294967295),
 intent_version bigint NOT NULL CHECK(intent_version>0),
 tenant_id text NOT NULL CHECK(tenant_id ~ '^[a-f0-9]{32}$'),
 timeline_id text NOT NULL CHECK(timeline_id ~ '^[a-f0-9]{32}$'),
 expectation jsonb NOT NULL CHECK(jsonb_typeof(expectation)='object'),
 marker_sha256 text NOT NULL CHECK(marker_sha256 ~ '^[a-f0-9]{64}$'),
 credential_ref text NOT NULL DEFAULT '',
 credential_uid text NOT NULL DEFAULT '',
 captured_by text NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(branch_id,kind,name),
 UNIQUE(branch_id,kind,object_oid),
 CHECK((kind='role' AND credential_ref<>'' AND credential_uid<>'') OR
       (kind='database' AND credential_ref='' AND credential_uid=''))
);
CREATE TABLE catalog_identity_requests (
 actor_id text NOT NULL REFERENCES users(id),
 key_hash text NOT NULL,
 request_hash text NOT NULL,
 branch_id text NOT NULL REFERENCES branches(id),
 response jsonb NOT NULL CHECK(jsonb_typeof(response)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(actor_id,key_hash)
);
