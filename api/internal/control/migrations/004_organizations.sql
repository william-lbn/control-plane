CREATE TABLE IF NOT EXISTS organizations (
    id text PRIMARY KEY,
    slug text NOT NULL UNIQUE,
    name text NOT NULL,
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','suspended')),
    created_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO organizations(id,slug,name) VALUES('local','local','Local workspace') ON CONFLICT(id) DO NOTHING;
CREATE TABLE IF NOT EXISTS organization_members (
    org_id text NOT NULL REFERENCES organizations(id),
    user_id text NOT NULL REFERENCES users(id),
    role text NOT NULL CHECK (role IN ('owner','admin','developer','viewer')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(org_id,user_id)
);
CREATE TABLE IF NOT EXISTS project_grants (
    project_id text NOT NULL REFERENCES projects(id),
    user_id text NOT NULL REFERENCES users(id),
    role text NOT NULL CHECK (role IN ('admin','developer','viewer')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(project_id,user_id)
);
ALTER TABLE projects ADD CONSTRAINT projects_org_fk FOREIGN KEY(org_id) REFERENCES organizations(id);
