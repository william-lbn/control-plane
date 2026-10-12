-- Keep immutable deleted definitions/deployments for evidence while permitting
-- a new Function identity to reuse the retired branch-local product slug.
-- SQL role, credentials and guest identities are never reused by this change.
ALTER TABLE function_definitions DROP CONSTRAINT function_definitions_project_id_branch_id_slug_key;
CREATE UNIQUE INDEX one_live_function_slug ON function_definitions(project_id,branch_id,slug) WHERE state<>'deleted';
