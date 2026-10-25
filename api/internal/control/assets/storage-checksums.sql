-- Additive format 5: retain old manifests and their unknown checksum fields.
-- New publication stores checksums in the same transaction as its blob identity.
ALTER TABLE neon_storage.objects ADD COLUMN IF NOT EXISTS checksums jsonb NOT NULL DEFAULT '{}'::jsonb
  CHECK(jsonb_typeof(checksums)='object' AND octet_length(checksums::text)<=1024);
