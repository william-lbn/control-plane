-- Format 6 raises the representable size only. Operator admission defaults
-- remain 64 MiB and existing byte references/rows are never rewritten.
ALTER TABLE neon_storage.installations ADD COLUMN IF NOT EXISTS format_version integer NOT NULL DEFAULT 1 CHECK(format_version>0);
ALTER TABLE neon_storage.objects DROP CONSTRAINT IF EXISTS objects_size_check;
ALTER TABLE neon_storage.objects ADD CONSTRAINT objects_size_check CHECK(size BETWEEN 0 AND 5368709120);
ALTER TABLE neon_storage.multipart_uploads DROP CONSTRAINT IF EXISTS multipart_uploads_expected_size_check;
ALTER TABLE neon_storage.multipart_uploads ADD CONSTRAINT multipart_uploads_expected_size_check CHECK(expected_size BETWEEN -1 AND 5368709120);
