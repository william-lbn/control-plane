-- Additive format 7. Legacy rows and completion request hashes are preserved.
ALTER TABLE neon_storage.objects ADD COLUMN IF NOT EXISTS checksum_policy jsonb NOT NULL DEFAULT '{}'::jsonb
  CHECK(jsonb_typeof(checksum_policy)='object' AND octet_length(checksum_policy::text)<=512);
ALTER TABLE neon_storage.multipart_uploads
  ADD COLUMN IF NOT EXISTS checksum_algorithm text NOT NULL DEFAULT '' CHECK(checksum_algorithm IN ('','CRC32','CRC32C','CRC64NVME','SHA1','SHA256')),
  ADD COLUMN IF NOT EXISTS checksum_type text NOT NULL DEFAULT '' CHECK(checksum_type IN ('','FULL_OBJECT','COMPOSITE')),
  ADD COLUMN IF NOT EXISTS result_checksum_policy jsonb NOT NULL DEFAULT '{}'::jsonb
    CHECK(jsonb_typeof(result_checksum_policy)='object' AND octet_length(result_checksum_policy::text)<=512);
ALTER TABLE neon_storage.multipart_parts ADD COLUMN IF NOT EXISTS checksums jsonb NOT NULL DEFAULT '{}'::jsonb
  CHECK(jsonb_typeof(checksums)='object' AND octet_length(checksums::text)<=1024);
