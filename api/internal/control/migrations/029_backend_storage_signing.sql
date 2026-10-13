-- Additive unified credentials: existing AI-only tokens retain their meaning.
ALTER TABLE backend_credentials DROP CONSTRAINT backend_credentials_scopes_check;
CREATE FUNCTION valid_backend_scopes(value text[]) RETURNS boolean
 LANGUAGE SQL IMMUTABLE AS $$
 SELECT cardinality(value) BETWEEN 1 AND 3
 AND value <@ ARRAY['ai_gateway:invoke','storage:read','storage:write']::text[]
 AND array_position(value,NULL) IS NULL
 AND cardinality(value)=(SELECT count(DISTINCT item) FROM unnest(value) item)
$$;
ALTER TABLE backend_credentials ADD CONSTRAINT backend_credentials_scopes_check CHECK(valid_backend_scopes(scopes));
CREATE TABLE backend_storage_signing (
 credential_id text PRIMARY KEY REFERENCES backend_credentials(id),
 generation bigint NOT NULL CHECK(generation>0),
 key_version text NOT NULL,
 sealed_token bytea NOT NULL CHECK(octet_length(sealed_token) BETWEEN 100 AND 512),
 updated_at timestamptz NOT NULL DEFAULT now()
);
-- The inference DB role receives no permission on this new encrypted table.
-- Ciphertext is retained on revoke; no plaintext is stored in replay/audit.
