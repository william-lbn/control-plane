ALTER TABLE realtime_source_runtime ADD COLUMN credential_namespace text NOT NULL DEFAULT 'neon'
 CHECK(credential_namespace ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$');
ALTER TABLE realtime_queries ADD COLUMN credential_namespace text NOT NULL DEFAULT 'neon'
 CHECK(credential_namespace ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$');
