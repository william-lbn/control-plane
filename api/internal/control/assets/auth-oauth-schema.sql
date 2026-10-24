-- A provider subject has one local owner, including simultaneous callbacks.
-- Conflicting legacy rows fail the additive upgrade without deleting users.
CREATE UNIQUE INDEX IF NOT EXISTS account_provider_identity_idx ON neon_auth.account("providerId","accountId");
