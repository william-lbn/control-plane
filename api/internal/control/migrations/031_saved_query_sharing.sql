-- Sharing is explicit and revocable. Existing personal drafts stay private.
ALTER TABLE saved_queries ADD COLUMN is_shared boolean NOT NULL DEFAULT false;
CREATE INDEX saved_queries_shared_project ON saved_queries(project_id,updated_at DESC,id) WHERE is_shared AND deleted_at IS NULL;
