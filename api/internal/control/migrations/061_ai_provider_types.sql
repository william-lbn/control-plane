-- Native discovery is an immutable revision input. Historical approvals and
-- keyed requests retain their OpenAI-compatible meaning without reinterpretation.
ALTER TABLE ai_provider_revisions ADD COLUMN provider_type text NOT NULL DEFAULT 'openai_compatible'
 CHECK(provider_type IN ('openai_compatible','anthropic','gemini'));
CREATE FUNCTION guard_ai_provider_type() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.provider_type IS DISTINCT FROM OLD.provider_type THEN
  RAISE EXCEPTION 'provider discovery type is immutable';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER ai_provider_type_guard BEFORE UPDATE ON ai_provider_revisions
 FOR EACH ROW EXECUTE FUNCTION guard_ai_provider_type();
-- Appending the column preserves the existing restricted view grants.
CREATE OR REPLACE VIEW ai_provider_runtime AS
 SELECT p.id AS provider_id, r.id AS revision_id, r.base_url,r.endpoint_sha256,
 r.credential_mode,r.credential_box,r.network,r.policy_sha256,pub.models,r.provider_type
 FROM ai_providers p JOIN ai_provider_publications pub ON pub.id=p.active_publication_id AND pub.provider_id=p.id
 JOIN ai_provider_revisions r ON r.id=pub.revision_id AND r.provider_id=p.id
 WHERE p.state='active' AND r.state='tested';
