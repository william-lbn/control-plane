-- Console factors are independent of branch application Auth. TOTP secrets use
-- the existing versioned envelope keyring; recovery codes are stored as hashes.
CREATE TABLE console_mfa (
    user_id text PRIMARY KEY REFERENCES users(id),
    key_version text NOT NULL,
    sealed_secret bytea NOT NULL,
    pending_until timestamptz,
    enabled_at timestamptz,
    last_step bigint NOT NULL DEFAULT -1,
    recovery_hashes jsonb NOT NULL DEFAULT '[]',
    CHECK ((enabled_at IS NULL) <> (pending_until IS NULL)),
    CHECK (jsonb_typeof(recovery_hashes) = 'array')
);
ALTER TABLE sessions ADD COLUMN mfa_verified_at timestamptz;

-- An older API must fail closed when inserting a password-only session for an
-- MFA account after rollback. Expired/revoked sessions are not revived here.
CREATE FUNCTION console_session_mfa_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM console_mfa WHERE user_id=NEW.user_id AND enabled_at IS NOT NULL
               AND (NEW.mfa_verified_at IS NULL OR NEW.mfa_verified_at < enabled_at)) THEN
        RAISE EXCEPTION 'console MFA proof required' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER console_session_mfa_guard BEFORE INSERT ON sessions
    FOR EACH ROW EXECUTE FUNCTION console_session_mfa_guard();
