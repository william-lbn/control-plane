-- Intent and observation are separate from PostgreSQL's live catalog.
-- Passwords and SCRAM verifiers live only in owned Kubernetes Secrets.
CREATE TABLE branch_roles (
    branch_id text NOT NULL REFERENCES branches(id),
    name text NOT NULL,
    state text NOT NULL CHECK (state IN ('creating','ready','updating','deleting','deleted')),
    credential_ref text,
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(branch_id,name)
);
CREATE TABLE branch_databases (
    branch_id text NOT NULL REFERENCES branches(id),
    name text NOT NULL,
    owner_name text NOT NULL,
    state text NOT NULL CHECK (state IN ('creating','ready','deleting','deleted')),
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(branch_id,name)
);
-- Catalog changes touch every endpoint of a branch. Serialize that resource.
CREATE UNIQUE INDEX one_active_branch_catalog_operation ON operations(resource_id)
    WHERE resource_type='branch_catalog' AND state IN ('queued','running','retry_wait');
