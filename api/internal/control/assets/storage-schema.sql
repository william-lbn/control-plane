-- The branch timeline owns this directory. Blobs are immutable and live in a
-- separate product bucket, so cloning this schema shares bytes without copying.
CREATE TABLE neon_storage.buckets (
    name text PRIMARY KEY,
    access text NOT NULL CHECK (access IN ('private','public_read')),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE neon_storage.objects (
    bucket text NOT NULL REFERENCES neon_storage.buckets(name),
    key text NOT NULL,
    blob_key text NOT NULL,
    sha256 text NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
    size bigint NOT NULL CHECK (size BETWEEN 0 AND 8388608),
    content_type text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(bucket,key)
);
CREATE TABLE neon_storage.installations (
    branch_id text PRIMARY KEY,
    generation bigint NOT NULL CHECK (generation > 0)
);
