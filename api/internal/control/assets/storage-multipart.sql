-- In-flight uploads belong to this branch, not to its descendants. The directory
-- and completion receipt commit together in this database: no cross-database
-- transaction or replay can publish an old upload over a subsequently edited key.
CREATE TABLE IF NOT EXISTS neon_storage.multipart_uploads (
    id text PRIMARY KEY,
    branch_id text NOT NULL,
    principal text NOT NULL,
    bucket text NOT NULL,
    key text NOT NULL,
    content_type text NOT NULL,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    expected_sha256 text NOT NULL DEFAULT '',
    expected_size bigint NOT NULL DEFAULT -1 CHECK (expected_size BETWEEN -1 AND 8388608),
    request_key text,
    request_hash text NOT NULL DEFAULT '',
    state text NOT NULL DEFAULT 'uploading' CHECK (state IN ('uploading','completed','aborted')),
    completion_hash text NOT NULL DEFAULT '',
    result_sha256 text NOT NULL DEFAULT '',
    result_size bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (branch_id,principal,request_key)
);
CREATE INDEX IF NOT EXISTS multipart_branch_owner ON neon_storage.multipart_uploads(branch_id,principal,bucket,key COLLATE "C",id) WHERE state='uploading';
CREATE TABLE IF NOT EXISTS neon_storage.multipart_parts (
    upload_id text NOT NULL REFERENCES neon_storage.multipart_uploads(id),
    part_number integer NOT NULL CHECK (part_number BETWEEN 1 AND 10000),
    blob_key text NOT NULL,
    sha256 text NOT NULL,
    etag text NOT NULL,
    size bigint NOT NULL CHECK (size BETWEEN 0 AND 8388608),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(upload_id,part_number)
);
