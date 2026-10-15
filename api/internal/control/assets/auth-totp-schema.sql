-- Additive schema generated against pinned Better Auth 1.7.7 twoFactor options.
-- Apply only inside an already verified control-owned neon_auth schema.
ALTER TABLE neon_auth."user" ADD COLUMN IF NOT EXISTS "twoFactorEnabled" boolean;
CREATE TABLE IF NOT EXISTS neon_auth."twoFactor" (
 "id" text NOT NULL PRIMARY KEY,
 "secret" text NOT NULL,
 "backupCodes" text NOT NULL,
 "userId" text NOT NULL REFERENCES neon_auth."user" ("id") ON DELETE CASCADE,
 "verified" boolean,
 "failedVerificationCount" integer,
 "lockedUntil" timestamptz
);
CREATE INDEX IF NOT EXISTS "twoFactor_secret_idx" ON neon_auth."twoFactor" ("secret");
CREATE INDEX IF NOT EXISTS "twoFactor_userId_idx" ON neon_auth."twoFactor" ("userId");
