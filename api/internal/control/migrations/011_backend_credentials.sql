-- Backend application credentials are independent of Console/API credentials.
-- Secret values never enter metadata, replay records or Operation payloads.
CREATE UNIQUE INDEX branches_identity_project ON branches(id,project_id);
CREATE TABLE backend_credentials (
    id text PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects(id),
    org_id text NOT NULL REFERENCES organizations(id),
    branch_id text NOT NULL,
    issuer_id text NOT NULL REFERENCES users(id),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    scopes text[] NOT NULL CHECK (scopes = ARRAY['ai_gateway:invoke']::text[]),
    branch_scope text NOT NULL CHECK (branch_scope IN ('self','self_and_descendants')),
    allowed_models text[] NOT NULL DEFAULT '{}',
    token_hash text NOT NULL CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    pepper_version text NOT NULL,
    generation bigint NOT NULL DEFAULT 1 CHECK (generation > 0),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    rotated_at timestamptz,
    last_used_at timestamptz,
    FOREIGN KEY(branch_id,project_id) REFERENCES branches(id,project_id)
);
CREATE INDEX backend_credentials_branch ON backend_credentials(project_id,branch_id,created_at DESC,id);
CREATE TABLE backend_credential_requests (
    actor_id text NOT NULL REFERENCES users(id),
    key_hash text NOT NULL,
    request_hash text NOT NULL,
    response jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(actor_id,key_hash)
);
