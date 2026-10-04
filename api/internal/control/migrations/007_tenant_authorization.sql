ALTER TABLE organization_members DROP CONSTRAINT organization_members_role_check;
UPDATE organization_members SET role='editor' WHERE role='developer';
ALTER TABLE organization_members ADD CONSTRAINT organization_members_role_check
    CHECK (role IN ('owner','admin','editor','viewer','collaborator'));
ALTER TABLE project_grants DROP CONSTRAINT project_grants_role_check;
UPDATE project_grants SET role='editor' WHERE role='developer';
ALTER TABLE project_grants ADD CONSTRAINT project_grants_role_check
    CHECK (role IN ('admin','editor','viewer'));
INSERT INTO project_grants(project_id,user_id,role)
    SELECT DISTINCT p.id,o.actor_id,'admin' FROM projects p
    JOIN operations o ON o.resource_id=p.id AND o.action='create_project'
    JOIN organization_members m ON m.org_id=p.org_id AND m.user_id=o.actor_id
    ON CONFLICT(project_id,user_id) DO NOTHING;

CREATE TABLE api_keys (
    id text PRIMARY KEY,
    user_id text NOT NULL REFERENCES users(id),
    org_id text REFERENCES organizations(id),
    project_id text REFERENCES projects(id),
    name text NOT NULL,
    key_hash text NOT NULL UNIQUE,
    max_role text NOT NULL CHECK (max_role IN ('viewer','editor','admin')),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    CHECK (project_id IS NULL OR org_id IS NOT NULL)
);
CREATE INDEX api_keys_owner ON api_keys(user_id) WHERE revoked_at IS NULL;
CREATE INDEX api_keys_org ON api_keys(org_id) WHERE revoked_at IS NULL;
CREATE TABLE organization_quotas (
    org_id text PRIMARY KEY REFERENCES organizations(id),
    max_projects integer NOT NULL DEFAULT 50 CHECK (max_projects > 0),
    max_endpoints integer NOT NULL DEFAULT 200 CHECK (max_endpoints > 0)
);
INSERT INTO organization_quotas(org_id) SELECT id FROM organizations ON CONFLICT DO NOTHING;
