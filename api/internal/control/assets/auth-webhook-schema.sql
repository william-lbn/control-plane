CREATE TABLE IF NOT EXISTS neon_auth.control_webhook_policy (
 singleton boolean PRIMARY KEY CHECK(singleton), enabled boolean NOT NULL,
 branch_id text NOT NULL, project_id text NOT NULL, endpoint_id text NOT NULL,
 generation bigint NOT NULL CHECK(generation>0), events text[] NOT NULL,
 dropped_events bigint NOT NULL DEFAULT 0 CHECK(dropped_events>=0)
);
CREATE TABLE IF NOT EXISTS neon_auth.control_webhook_event (
 id uuid PRIMARY KEY, branch_id text NOT NULL, generation bigint NOT NULL,
 event_type text NOT NULL CHECK(event_type IN ('user.created','phone_number.verified','organization.invitation.created','organization.invitation.accepted')),
 payload jsonb NOT NULL CHECK(octet_length(payload::text)<=32768),
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','delivering','delivered','failed','discarded')),
 delivery_batches integer NOT NULL DEFAULT 0 CHECK(delivery_batches BETWEEN 0 AND 3),
 lease_token text, lease_until timestamptz, last_error text,
 created_at timestamptz NOT NULL DEFAULT now(), delivered_at timestamptz
);
CREATE INDEX IF NOT EXISTS control_webhook_delivery_idx ON neon_auth.control_webhook_event(branch_id,generation,state,created_at);
CREATE OR REPLACE FUNCTION neon_auth.control_webhook_user(value jsonb) RETURNS jsonb
LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $$
 SELECT COALESCE(jsonb_object_agg(target,value->source),'{}'::jsonb)
 FROM (VALUES ('id','id'),('name','name'),('email','email'),('image','image'),('role','role'),('banned','banned'),
 ('emailVerified','email_verified'),('phoneNumber','phone_number'),('phoneNumberVerified','phone_number_verified'),
 ('createdAt','created_at'),('updatedAt','updated_at'),('banReason','ban_reason'),('banExpires','ban_expires'),
 ('twoFactorEnabled','two_factor_enabled'),('isAnonymous','is_anonymous')) fields(source,target)
 WHERE jsonb_typeof(value->source)='boolean' OR
 (jsonb_typeof(value->source)='string' AND length(value->>source)<=CASE WHEN source='image' THEN 2048 ELSE 500 END)
$$;
REVOKE ALL ON FUNCTION neon_auth.control_webhook_user(jsonb) FROM PUBLIC;
CREATE OR REPLACE FUNCTION neon_auth.control_webhook_enqueue() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,neon_auth AS $$
DECLARE
 policy neon_auth.control_webhook_policy%ROWTYPE;
 event_id uuid; event_type text; actor jsonb; data jsonb; org jsonb; membership text;
BEGIN
 SELECT * INTO policy FROM neon_auth.control_webhook_policy WHERE singleton;
 IF NOT FOUND OR NOT policy.enabled THEN RETURN NEW; END IF;
 -- Always take the admission lock before the policy row lock. In particular,
 -- a full queue must not deadlock two producers upgrading their row locks.
 PERFORM pg_advisory_xact_lock(hashtextextended('neon-auth-webhook:'||policy.branch_id,922751));
 SELECT * INTO policy FROM neon_auth.control_webhook_policy WHERE singleton FOR SHARE;
 IF NOT FOUND OR NOT policy.enabled THEN RETURN NEW; END IF;
 IF TG_TABLE_NAME='user' THEN
  actor:=to_jsonb(NEW);
  IF TG_OP='INSERT' THEN event_type:='user.created'; data:='{}'::jsonb;
  ELSIF COALESCE(actor->>'phoneNumberVerified','false')='true' AND
        current_setting('neon_control.auth_action',true)='phone-number/verify' THEN
   event_type:='phone_number.verified'; data:=jsonb_build_object('phone_number',actor->>'phoneNumber');
  ELSE RETURN NEW; END IF;
 ELSE
  SELECT to_jsonb(u) INTO actor FROM neon_auth."user" u WHERE id=NEW."inviterId";
  SELECT to_jsonb(o) INTO org FROM neon_auth.organization o WHERE id=NEW."organizationId";
  IF TG_OP='INSERT' THEN
   event_type:='organization.invitation.created';
   data:=jsonb_build_object('invitation_id',NEW.id,'organization_id',NEW."organizationId",'invitee_email',NEW.email,
     'role',NEW.role,'expires_at',NEW."expiresAt",'organization_name',left(org->>'name',500),'organization_slug',left(org->>'slug',500));
  ELSIF NEW.status='accepted' AND OLD.status<>'accepted' THEN
   -- Deferred until COMMIT: the maintained plugin updates the invitation
   -- before inserting its member. Failed quota admissions emit no event.
   SELECT m.id,to_jsonb(u) INTO membership,actor FROM neon_auth.member m
    JOIN neon_auth."user" u ON u.id=m."userId"
    WHERE m."organizationId"=NEW."organizationId" AND lower(u.email)=lower(NEW.email);
   IF membership IS NULL THEN RETURN NEW; END IF;
   event_type:='organization.invitation.accepted';
   data:=jsonb_build_object('invitation_id',NEW.id,'organization_id',NEW."organizationId",'member_id',membership,
     'role',NEW.role,'organization_name',left(org->>'name',500),'organization_slug',left(org->>'slug',500));
  ELSE RETURN NEW; END IF;
 END IF;
 IF NOT event_type=ANY(policy.events) THEN RETURN NEW; END IF;
 -- Serialize only queue admission. A full nonblocking queue must not prevent
 -- the application mutation; retain an explicit drop counter instead.
 IF (SELECT count(*) FROM neon_auth.control_webhook_event WHERE branch_id=policy.branch_id AND
     generation=policy.generation AND state IN ('pending','delivering'))>=1000 THEN
  UPDATE neon_auth.control_webhook_policy SET dropped_events=dropped_events+1 WHERE singleton;
  RETURN NEW;
 END IF;
 event_id:=gen_random_uuid();
 INSERT INTO neon_auth.control_webhook_event(id,branch_id,generation,event_type,payload)
 VALUES(event_id,policy.branch_id,policy.generation,event_type,jsonb_build_object(
  'event_id',event_id::text,'event_type',event_type,'timestamp',to_jsonb(clock_timestamp()),
  'context',jsonb_build_object('branch_id',policy.branch_id,'project_id',policy.project_id,'endpoint_id',policy.endpoint_id),
  'user',neon_auth.control_webhook_user(COALESCE(actor,'{}'::jsonb)),'event_data',data));
 RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION neon_auth.control_webhook_enqueue() FROM PUBLIC;
DROP TRIGGER IF EXISTS control_webhook_user_created ON neon_auth."user";
CREATE TRIGGER control_webhook_user_created AFTER INSERT ON neon_auth."user" FOR EACH ROW EXECUTE FUNCTION neon_auth.control_webhook_enqueue();
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='neon_auth' AND table_name='user' AND column_name='phoneNumberVerified') THEN
  DROP TRIGGER IF EXISTS control_webhook_phone_verified ON neon_auth."user";
  CREATE TRIGGER control_webhook_phone_verified AFTER UPDATE OF "phoneNumberVerified" ON neon_auth."user" FOR EACH ROW EXECUTE FUNCTION neon_auth.control_webhook_enqueue();
 END IF;
 IF to_regclass('neon_auth.invitation') IS NOT NULL THEN
  DROP TRIGGER IF EXISTS control_webhook_invitation_created ON neon_auth.invitation;
  CREATE TRIGGER control_webhook_invitation_created AFTER INSERT ON neon_auth.invitation FOR EACH ROW EXECUTE FUNCTION neon_auth.control_webhook_enqueue();
  DROP TRIGGER IF EXISTS control_webhook_invitation_accepted ON neon_auth.invitation;
  CREATE CONSTRAINT TRIGGER control_webhook_invitation_accepted AFTER UPDATE ON neon_auth.invitation
   DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION neon_auth.control_webhook_enqueue();
 END IF;
END $$;
