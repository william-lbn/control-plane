-- 原端点Operation的唯一策略代次，未知外部写入时保留原意图。
CREATE TABLE dsga_runtime_policy_intents (
 operation_id text PRIMARY KEY REFERENCES operations(id),
 intent jsonb NOT NULL CHECK(jsonb_typeof(intent)='object' AND octet_length(intent::text)<=65536),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE FUNCTION guard_dsga_policy_intent() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP<>'INSERT' OR NOT EXISTS(SELECT 1 FROM operations WHERE id=NEW.operation_id
   AND action='update_endpoint' AND state='running' AND lease_expires_at>clock_timestamp()) THEN
  RAISE EXCEPTION 'policy intent requires original active operation and is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER dsga_policy_intent_guard BEFORE INSERT OR UPDATE OR DELETE ON dsga_runtime_policy_intents
 FOR EACH ROW EXECUTE FUNCTION guard_dsga_policy_intent();