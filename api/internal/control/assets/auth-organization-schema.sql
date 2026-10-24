-- Maintained Better Auth 1.7.7 organization fields; extra owned policy/guards
-- enforce quotas and owner retention in the same native transaction.
CREATE TABLE IF NOT EXISTS neon_auth.organization (
 id text PRIMARY KEY, name text NOT NULL, slug text NOT NULL UNIQUE,
 logo text, "createdAt" timestamptz NOT NULL, metadata text
);
CREATE TABLE IF NOT EXISTS neon_auth.member (
 id text PRIMARY KEY,
 "organizationId" text NOT NULL REFERENCES neon_auth.organization(id) ON DELETE CASCADE,
 "userId" text NOT NULL REFERENCES neon_auth."user"(id) ON DELETE CASCADE,
 role text NOT NULL DEFAULT 'member', "createdAt" timestamptz NOT NULL,
 UNIQUE("organizationId","userId")
);
CREATE INDEX IF NOT EXISTS member_user_idx ON neon_auth.member("userId");
CREATE INDEX IF NOT EXISTS member_org_idx ON neon_auth.member("organizationId");
CREATE TABLE IF NOT EXISTS neon_auth.invitation (
 id text PRIMARY KEY,
 "organizationId" text NOT NULL REFERENCES neon_auth.organization(id) ON DELETE CASCADE,
 email text NOT NULL, role text, status text NOT NULL DEFAULT 'pending',
 "expiresAt" timestamptz NOT NULL, "createdAt" timestamptz NOT NULL DEFAULT now(),
 "inviterId" text NOT NULL REFERENCES neon_auth."user"(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS invitation_org_idx ON neon_auth.invitation("organizationId");
CREATE INDEX IF NOT EXISTS invitation_email_idx ON neon_auth.invitation(email);
ALTER TABLE neon_auth.session ADD COLUMN IF NOT EXISTS "activeOrganizationId" text;
CREATE TABLE IF NOT EXISTS neon_auth.control_organization_policy (
 singleton boolean PRIMARY KEY CHECK(singleton), enabled boolean NOT NULL,
 user_memberships integer NOT NULL CHECK(user_memberships BETWEEN 1 AND 100),
 organization_members integer NOT NULL CHECK(organization_members BETWEEN 1 AND 1000),
 branch_id text NOT NULL,
 generation bigint NOT NULL CHECK(generation>0)
);
CREATE OR REPLACE FUNCTION neon_auth.control_organization_member_guard() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,neon_auth AS $$
DECLARE
 org text; policy neon_auth.control_organization_policy%ROWTYPE;
BEGIN
 IF TG_OP='UPDATE' AND (NEW."organizationId"<>OLD."organizationId" OR NEW."userId"<>OLD."userId") THEN
  RAISE EXCEPTION USING ERRCODE='P4004',MESSAGE='Organization member identity is immutable';
 END IF;
 IF TG_OP<>'DELETE' AND (NEW.role='' OR EXISTS(
  SELECT 1 FROM unnest(string_to_array(NEW.role,',')) r WHERE r NOT IN ('owner','admin','member')
 )) THEN RAISE EXCEPTION USING ERRCODE='P4004',MESSAGE='Invalid organization role'; END IF;
 IF TG_OP='INSERT' THEN
  SELECT * INTO policy FROM neon_auth.control_organization_policy WHERE singleton FOR SHARE;
  IF NOT FOUND OR NOT policy.enabled THEN
   RAISE EXCEPTION USING ERRCODE='P4005',MESSAGE='Organization management disabled';
  END IF;
  PERFORM pg_advisory_xact_lock(hashtextextended('neon-auth-org-user:'||NEW."userId",922751));
  PERFORM pg_advisory_xact_lock(hashtextextended('neon-auth-org:'||NEW."organizationId",922751));
  IF (SELECT count(*) FROM neon_auth.member WHERE "userId"=NEW."userId")>=policy.user_memberships THEN
   RAISE EXCEPTION USING ERRCODE='P4001',MESSAGE='Organization membership limit reached';
  END IF;
  IF (SELECT count(*) FROM neon_auth.member WHERE "organizationId"=NEW."organizationId")>=policy.organization_members THEN
   RAISE EXCEPTION USING ERRCODE='P4002',MESSAGE='Organization member limit reached';
  END IF;
 END IF;
 IF TG_OP='DELETE' OR (TG_OP='UPDATE' AND 'owner'=ANY(string_to_array(OLD.role,',')) AND NOT 'owner'=ANY(string_to_array(NEW.role,','))) THEN
  org:=OLD."organizationId";
  PERFORM pg_advisory_xact_lock(hashtextextended('neon-auth-org:'||org,922751));
  IF 'owner'=ANY(string_to_array(OLD.role,','))
     AND EXISTS(SELECT 1 FROM neon_auth.organization WHERE id=org)
     AND COALESCE(current_setting('neon_control.auth_deleting_organization',true),'')<>org
     AND NOT EXISTS(SELECT 1 FROM neon_auth.member WHERE "organizationId"=org AND id<>OLD.id AND 'owner'=ANY(string_to_array(role,','))) THEN
   RAISE EXCEPTION USING ERRCODE='P4003',MESSAGE='Organization must retain an owner';
  END IF;
 END IF;
 IF TG_OP='DELETE' OR (TG_OP='UPDATE' AND string_to_array(OLD.role,',')&&ARRAY['owner','admin'] AND NOT string_to_array(NEW.role,',')&&ARRAY['owner','admin']) THEN
  org:=OLD."organizationId";
  PERFORM pg_advisory_xact_lock(hashtextextended('neon-auth-org:'||org,922751));
  IF string_to_array(OLD.role,',')&&ARRAY['owner','admin']
     AND EXISTS(SELECT 1 FROM neon_auth.organization WHERE id=org)
     AND COALESCE(current_setting('neon_control.auth_deleting_organization',true),'')<>org
     AND NOT EXISTS(SELECT 1 FROM neon_auth.member WHERE "organizationId"=org AND id<>OLD.id AND string_to_array(role,',')&&ARRAY['owner','admin']) THEN
   RAISE EXCEPTION USING ERRCODE='P4006',MESSAGE='Organization must retain a manager';
  END IF;
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION neon_auth.control_organization_member_guard() FROM PUBLIC;
DROP TRIGGER IF EXISTS control_organization_member_guard ON neon_auth.member;
CREATE TRIGGER control_organization_member_guard BEFORE INSERT OR UPDATE OR DELETE ON neon_auth.member
FOR EACH ROW EXECUTE FUNCTION neon_auth.control_organization_member_guard();
