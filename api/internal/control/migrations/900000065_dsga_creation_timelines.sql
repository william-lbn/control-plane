-- 全分片创建在发出HTTP前保存完整原请求；NULL确认时间只表示未确认，不能表示
-- 未执行或允许清理。只允许首次确认，不更新原资源/版本/祖先/LSN，不靠步骤状态。
CREATE TABLE dsga_creation_timelines (
 operation_id text PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
 tenant_id text NOT NULL CHECK (tenant_id ~ '^[0-9a-f]{32}$'),
 timeline_id text NOT NULL CHECK (timeline_id ~ '^[0-9a-f]{32}$'),
 request jsonb NOT NULL CHECK (jsonb_typeof(request)='object'),
 confirmed_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(tenant_id,timeline_id)
);

CREATE FUNCTION guard_dsga_creation_timeline() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE original operations%ROWTYPE; expected jsonb;
BEGIN
 IF TG_OP='DELETE' THEN
   IF EXISTS(SELECT 1 FROM operations WHERE id=OLD.operation_id) THEN
     RAISE EXCEPTION 'DSGA original timeline intent is immutable' USING ERRCODE='23514';
   END IF;
   RETURN OLD;
 END IF;
 IF TG_OP='UPDATE' THEN
   IF OLD.confirmed_at IS NOT NULL OR NEW.confirmed_at IS NULL OR
      (to_jsonb(NEW)-'confirmed_at') IS DISTINCT FROM (to_jsonb(OLD)-'confirmed_at') THEN
     RAISE EXCEPTION 'DSGA only first timeline confirmation is allowed' USING ERRCODE='23514';
   END IF;
   NEW.confirmed_at:=clock_timestamp();
 ELSIF NEW.confirmed_at IS NOT NULL THEN
   RAISE EXCEPTION 'DSGA timeline must retain original unconfirmed intent first' USING ERRCODE='23514';
 END IF;
 -- 调用方先锁Operation→timeline；触发器同序读取。取得锁后再读真实时钟，
 -- 不能用事务开始now()将等待期间已经过期的lease当有效。
 SELECT * INTO original FROM operations WHERE id=NEW.operation_id FOR SHARE;
 IF NOT FOUND OR original.action NOT IN ('create_project_dsga_v1','create_branch_dsga_v1') OR
    original.state<>'running' OR original.lease_owner IS NULL OR
    original.lease_expires_at IS NULL OR original.lease_expires_at<=clock_timestamp() OR
    original.payload->>'tenant_id' IS DISTINCT FROM NEW.tenant_id OR
    original.payload->>'timeline_id' IS DISTINCT FROM NEW.timeline_id OR
    original.payload#>>'{dsga_creation,postgres_version}' IS DISTINCT FROM '17' OR
    original.payload#>>'{dsga_creation,protocol_version}' IS DISTINCT FROM '1' THEN
   RAISE EXCEPTION 'DSGA original timeline identity and live lease required' USING ERRCODE='23514';
 END IF;
 expected:=jsonb_build_object('new_timeline_id',NEW.timeline_id,'pg_version',17);
 IF COALESCE(original.payload->>'parent_timeline_id','')<>'' THEN
   expected:=expected||jsonb_build_object('ancestor_timeline_id',original.payload->>'parent_timeline_id',
      'ancestor_start_lsn',original.payload->>'parent_lsn');
 END IF;
 IF NEW.request IS DISTINCT FROM expected OR
    (original.action='create_project_dsga_v1' AND COALESCE(original.payload->>'parent_timeline_id','')<>'') OR
    (original.action='create_branch_dsga_v1' AND COALESCE(original.payload->>'parent_timeline_id','')='') THEN
   RAISE EXCEPTION 'DSGA frozen timeline request differs' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER dsga_creation_timeline_guard BEFORE INSERT OR UPDATE OR DELETE ON dsga_creation_timelines
 FOR EACH ROW EXECUTE FUNCTION guard_dsga_creation_timeline();
