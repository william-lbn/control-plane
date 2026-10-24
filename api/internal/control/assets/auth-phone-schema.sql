-- Phone identity remains ordinary branch user data. Challenges are temporary
-- and contain keyed digests, never an SMS code or a session credential.
ALTER TABLE neon_auth."user" ADD COLUMN IF NOT EXISTS "phoneNumber" text;
ALTER TABLE neon_auth."user" ADD COLUMN IF NOT EXISTS "phoneNumberVerified" boolean NOT NULL DEFAULT false;
CREATE UNIQUE INDEX IF NOT EXISTS user_phone_number_unique ON neon_auth."user"("phoneNumber") WHERE "phoneNumber" IS NOT NULL;
CREATE TABLE IF NOT EXISTS neon_auth.control_phone_challenge (
 phone text PRIMARY KEY CHECK(phone ~ '^\+[1-9][0-9]{7,14}$'),
 digest text NOT NULL CHECK(digest ~ '^[0-9a-f]{64}$'),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 2),
 expires_at timestamptz NOT NULL
);
