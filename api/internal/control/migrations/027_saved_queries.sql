-- Personal SQL templates, encrypted at rest; connection passwords/results are
-- never part of this model. Keep tombstones and metadata-only replay receipts.
CREATE TABLE saved_queries (
 id text PRIMARY KEY,
 project_id text NOT NULL REFERENCES projects(id),
 owner_id text NOT NULL REFERENCES users(id),
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 100),
 generation bigint NOT NULL DEFAULT 1 CHECK(generation>0),
 content_generation bigint NOT NULL DEFAULT 1 CHECK(content_generation>0 AND content_generation<=generation),
 key_version text NOT NULL,
 sealed_sql bytea NOT NULL CHECK(octet_length(sealed_sql) BETWEEN 28 AND 65564),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 deleted_at timestamptz
);
CREATE INDEX saved_queries_owner_project ON saved_queries(owner_id,project_id,updated_at DESC) WHERE deleted_at IS NULL;
CREATE TABLE saved_query_requests (
 owner_id text NOT NULL REFERENCES users(id),
 key_hash text NOT NULL,
 request_hash text NOT NULL,
 response jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(owner_id,key_hash)
);
