-- READ ONLY inventory component, not a native format migration. Execute inside
-- the native adapter's verified project/timeline/database snapshot and fence.
-- This query is NOT by itself a proof about retained PostgreSQL history.
-- UNION ALL preserves shared references and all multipart states/branches.
SELECT * FROM (
SELECT blob_key, 'object'::text AS reference_kind,
       bucket AS reference_owner, key AS reference_member
  FROM neon_storage.objects
UNION ALL
SELECT p.blob_key, 'multipart_part'::text,
       p.upload_id, p.part_number::text
  FROM neon_storage.multipart_parts p
) AS all_references
ORDER BY blob_key COLLATE "C", reference_kind, reference_owner, reference_member;
