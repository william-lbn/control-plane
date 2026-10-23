-- The layout belongs to this published object generation, never a mutable
-- upload/key lookup. Empty historical layouts are left unknown, not inferred.
ALTER TABLE neon_storage.objects ADD COLUMN IF NOT EXISTS part_layout jsonb NOT NULL DEFAULT '[]'::jsonb
  CHECK(jsonb_typeof(part_layout)='array' AND jsonb_array_length(part_layout)<=1000);
