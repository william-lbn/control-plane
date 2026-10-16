-- Personal annotations are encrypted independently from immutable execution SQL.
-- Receipts deliberately contain only identifiers/version/has_name, not names.
ALTER TABLE sql_query_history ADD COLUMN name_generation bigint NOT NULL DEFAULT 0 CHECK (name_generation>=0);
ALTER TABLE sql_query_history ADD COLUMN name_key_version text;
ALTER TABLE sql_query_history ADD COLUMN sealed_name bytea;
ALTER TABLE sql_query_history ADD COLUMN name_updated_at timestamptz;
ALTER TABLE sql_query_history ADD CONSTRAINT sql_history_name_envelope CHECK (
 (name_key_version IS NULL AND sealed_name IS NULL) OR
 (name_key_version IS NOT NULL AND length(name_key_version)>0 AND sealed_name IS NOT NULL AND octet_length(sealed_name) BETWEEN 29 AND 268)
);
ALTER TABLE sql_query_history ADD CONSTRAINT sql_history_name_initial CHECK (
 (name_generation>0 AND name_updated_at IS NOT NULL) OR
 (name_generation=0 AND name_key_version IS NULL AND sealed_name IS NULL AND name_updated_at IS NULL)
);
ALTER TABLE sql_query_history ADD CONSTRAINT sql_history_name_owner UNIQUE(id,owner_id);
CREATE TABLE sql_history_name_requests (
 owner_id text NOT NULL REFERENCES users(id),
 history_id text NOT NULL,
 key_hash text NOT NULL,
 request_hash text NOT NULL,
 response jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(owner_id,history_id,key_hash),
 FOREIGN KEY(history_id,owner_id) REFERENCES sql_query_history(id,owner_id) ON DELETE CASCADE
);
CREATE INDEX sql_history_name_requests_rate ON sql_history_name_requests(owner_id,history_id,created_at);
