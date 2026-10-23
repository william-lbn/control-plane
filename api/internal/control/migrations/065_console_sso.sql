-- Console identity is explicitly linked to an existing account. Email/role
-- claims never create membership or grant control-plane permissions.
CREATE TABLE console_sso_policies (
 org_id text PRIMARY KEY REFERENCES organizations(id),
 provider_id text NOT NULL,
 provider_revision text NOT NULL,
 mode text NOT NULL CHECK(mode IN ('optional','required','disabled')),
 version bigint NOT NULL DEFAULT 1 CHECK(version>0),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE console_sso_bindings (
 id text PRIMARY KEY,
 org_id text NOT NULL REFERENCES organizations(id),
 provider_id text NOT NULL,
 provider_revision text NOT NULL,
 subject text NOT NULL CHECK(length(subject) BETWEEN 1 AND 255),
 user_id text NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT now(),
 revoked_at timestamptz,
 UNIQUE(provider_revision,subject),
 UNIQUE(provider_revision,user_id)
);
CREATE TABLE console_sso_flows (
 state_hash text PRIMARY KEY,
 browser_hash text NOT NULL,
 nonce_hash text NOT NULL,
 key_version text NOT NULL,
 sealed_verifier bytea NOT NULL,
 org_id text NOT NULL REFERENCES organizations(id),
 provider_id text NOT NULL,
 provider_revision text NOT NULL,
 policy_version bigint NOT NULL,
 purpose text NOT NULL CHECK(purpose IN ('login','link')),
 user_id text REFERENCES users(id),
 session_hash text,
 expires_at timestamptz NOT NULL,
 used_at timestamptz,
 CHECK((purpose='link')=(user_id IS NOT NULL AND session_hash IS NOT NULL))
);
CREATE INDEX console_sso_flows_expiry ON console_sso_flows(expires_at);
CREATE TABLE console_sso_challenges (
 challenge_hash text PRIMARY KEY,
 binding_id text NOT NULL REFERENCES console_sso_bindings(id),
 policy_version bigint NOT NULL,
 expires_at timestamptz NOT NULL,
 token_expires_at timestamptz NOT NULL,
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts>=0),
 used_at timestamptz
);
CREATE INDEX console_sso_challenges_expiry ON console_sso_challenges(expires_at);
ALTER TABLE sessions ADD COLUMN sso_binding_id text REFERENCES console_sso_bindings(id);
ALTER TABLE sessions ADD COLUMN sso_policy_version bigint;
ALTER TABLE sessions ADD CHECK((sso_binding_id IS NULL)=(sso_policy_version IS NULL));
CREATE TABLE console_sso_policy_receipts (
 org_id text NOT NULL REFERENCES organizations(id),
 user_id text NOT NULL REFERENCES users(id),
 key_hash text NOT NULL,
 request_hash text NOT NULL,
 result jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(org_id,user_id,key_hash)
);
