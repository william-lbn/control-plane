-- Provider ownership cannot be moved after credentials have been sealed. NULL
-- retains the historical platform catalog and its existing operator boundary.
ALTER TABLE ai_providers ADD COLUMN org_id text REFERENCES organizations(id);
CREATE INDEX ai_providers_org ON ai_providers(org_id,state);
CREATE FUNCTION guard_ai_provider_organization() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.org_id IS DISTINCT FROM OLD.org_id THEN
  RAISE EXCEPTION 'provider organization ownership is immutable';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER ai_provider_organization_guard BEFORE UPDATE ON ai_providers
 FOR EACH ROW EXECUTE FUNCTION guard_ai_provider_organization();

-- Appended columns preserve the restricted Gateway's existing view grants.
CREATE OR REPLACE VIEW ai_provider_runtime AS
 SELECT p.id AS provider_id, r.id AS revision_id, r.base_url,r.endpoint_sha256,
 r.credential_mode,r.credential_box,r.network,r.policy_sha256,pub.models,r.provider_type,p.org_id
 FROM ai_providers p JOIN ai_provider_publications pub ON pub.id=p.active_publication_id AND pub.provider_id=p.id
 JOIN ai_provider_revisions r ON r.id=pub.revision_id AND r.provider_id=p.id
 WHERE p.state='active' AND r.state='tested'
 AND (p.org_id IS NULL OR EXISTS(SELECT 1 FROM organizations o WHERE o.id=p.org_id AND o.state='active'));

ALTER TABLE ai_gateway_requests ADD COLUMN billing_owner text NOT NULL DEFAULT 'platform'
 CHECK(billing_owner IN ('platform','organization_byok'));
ALTER TABLE ai_gateway_requests ADD COLUMN billing_org_id text REFERENCES organizations(id);
ALTER TABLE ai_gateway_requests ADD COLUMN provider_id text;
ALTER TABLE ai_gateway_requests ADD COLUMN provider_revision_id text;
ALTER TABLE ai_gateway_requests ADD FOREIGN KEY(provider_id,provider_revision_id)
 REFERENCES ai_provider_revisions(provider_id,id);
ALTER TABLE ai_gateway_requests ADD FOREIGN KEY(project_id,billing_org_id) REFERENCES projects(id,org_id);
ALTER TABLE ai_gateway_requests ADD CHECK((billing_owner='organization_byok')=(billing_org_id IS NOT NULL));
ALTER TABLE ai_gateway_requests ADD CHECK((provider_id IS NULL)=(provider_revision_id IS NULL));
ALTER TABLE ai_gateway_requests ADD CHECK(billing_owner<>'organization_byok' OR provider_id IS NOT NULL);
CREATE FUNCTION guard_ai_request_billing_owner() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE actual_org text;
BEGIN
 IF TG_OP='INSERT' THEN
  IF NEW.provider_id IS NOT NULL THEN
   SELECT org_id INTO actual_org FROM ai_provider_runtime WHERE provider_id=NEW.provider_id AND revision_id=NEW.provider_revision_id
    AND EXISTS(SELECT 1 FROM jsonb_array_elements(models) m WHERE m->>'id'=NEW.model_id);
   IF NOT FOUND OR NEW.billing_org_id IS DISTINCT FROM actual_org THEN
    RAISE EXCEPTION 'AI request billing owner must match immutable provider ownership';
   END IF;
  END IF;
 ELSIF (NEW.billing_owner,NEW.billing_org_id,NEW.provider_id,NEW.provider_revision_id)
 IS DISTINCT FROM (OLD.billing_owner,OLD.billing_org_id,OLD.provider_id,OLD.provider_revision_id) THEN
  RAISE EXCEPTION 'AI request billing attribution is immutable';
 END IF;
 RETURN NEW;
END $$;
-- Validation reads only the already granted runtime view; no elevated function.
CREATE TRIGGER ai_request_billing_owner_guard BEFORE INSERT OR UPDATE ON ai_gateway_requests
 FOR EACH ROW EXECUTE FUNCTION guard_ai_request_billing_owner();
