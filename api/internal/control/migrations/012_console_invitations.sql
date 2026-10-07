-- Console invitations are distinct from application Auth identities.
-- Retain consumed/revoked invitations and audits; never persist the bearer token.
CREATE TABLE console_invitations (
    id text PRIMARY KEY,
    org_id text NOT NULL REFERENCES organizations(id),
    username text NOT NULL,
    role text NOT NULL CHECK (role IN ('admin','editor','viewer','collaborator')),
    invited_by text NOT NULL REFERENCES users(id),
    token_hash text NOT NULL UNIQUE,
    key_hash text NOT NULL,
    request_hash text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    accepted_at timestamptz,
    accepted_by text REFERENCES users(id),
    revoked_at timestamptz,
    UNIQUE(invited_by,key_hash),
    CHECK (expires_at > created_at),
    CHECK ((accepted_at IS NULL) = (accepted_by IS NULL)),
    CHECK (accepted_at IS NULL OR revoked_at IS NULL)
);
CREATE UNIQUE INDEX one_pending_console_invitation ON console_invitations(org_id,username)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;
CREATE INDEX console_invitations_org ON console_invitations(org_id,created_at DESC);
-- A shared atomic admission counter prevents concurrent anonymous signup attempts
-- from bypassing an in-memory/per-instance limiter. Store hashed peer addresses.
CREATE TABLE console_registration_limits (
    scope_hash text NOT NULL,
    window_at timestamptz NOT NULL,
    attempts integer NOT NULL CHECK (attempts > 0),
    PRIMARY KEY(scope_hash,window_at)
);
