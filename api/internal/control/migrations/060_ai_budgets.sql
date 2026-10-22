ALTER TABLE ai_gateway_requests ADD COLUMN protocol text NOT NULL DEFAULT 'chat_completions' CHECK(protocol IN ('chat_completions','responses','embeddings','messages','gemini'));
ALTER TABLE ai_gateway_requests ADD COLUMN cache_read_tokens bigint CHECK(cache_read_tokens>=0);
ALTER TABLE ai_gateway_requests ADD COLUMN cache_write_tokens bigint CHECK(cache_write_tokens>=0);

-- Self-hosted spending controls, not a payment/credit issuance system. Counters
-- survive policy edits, disabling, deletion and uncertain request expiry.
CREATE TABLE ai_budget_policies (
 branch_id text PRIMARY KEY REFERENCES branches(id),
 project_id text NOT NULL REFERENCES projects(id),
 version bigint NOT NULL CHECK(version>0),
 enabled boolean NOT NULL,
 currency text NOT NULL CHECK(currency='USD'),
 limit_micros bigint NOT NULL CHECK(limit_micros BETWEEN 0 AND 1000000000000),
 spent_micros bigint NOT NULL DEFAULT 0 CHECK(spent_micros>=0),
 reserved_micros bigint NOT NULL DEFAULT 0 CHECK(reserved_micros>=0),
 updated_by text NOT NULL REFERENCES users(id),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(branch_id,project_id) REFERENCES branches(id,project_id)
);
CREATE TABLE ai_budget_reservations (
 request_id text PRIMARY KEY REFERENCES ai_gateway_requests(id),
 branch_id text NOT NULL REFERENCES ai_budget_policies(branch_id),
 price jsonb NOT NULL CHECK(jsonb_typeof(price)='object'),
 reserved_micros bigint NOT NULL CHECK(reserved_micros>=0),
 state text NOT NULL CHECK(state IN ('reserved','uncertain','settled','released')),
 actual_micros bigint CHECK(actual_micros>=0),
 usage jsonb,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 settled_at timestamptz,
 CHECK((state IN ('settled','released'))=(actual_micros IS NOT NULL)),
 CHECK(state<>'released' OR actual_micros=0)
);
CREATE INDEX ai_budget_reservations_branch ON ai_budget_reservations(branch_id,state,created_at);
CREATE TABLE ai_budget_ledger (
 id text PRIMARY KEY,
 branch_id text NOT NULL REFERENCES ai_budget_policies(branch_id),
 request_id text NOT NULL REFERENCES ai_budget_reservations(request_id),
 kind text NOT NULL CHECK(kind IN ('reserve','settle','release','uncertain','reconcile')),
 reserved_delta bigint NOT NULL,
 spent_delta bigint NOT NULL,
 actor_id text REFERENCES users(id),
 evidence_sha256 text CHECK(evidence_sha256 ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(request_id,kind)
);
CREATE TABLE ai_budget_mutations (
 actor_id text NOT NULL REFERENCES users(id),
 key_hash text NOT NULL,
 request_hash text NOT NULL,
 response jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(actor_id,key_hash)
);
CREATE FUNCTION preserve_ai_budget_ledger() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'AI budget ledger is immutable' USING ERRCODE='23514';
END $$;
CREATE TRIGGER preserve_ai_budget_ledger BEFORE UPDATE OR DELETE ON ai_budget_ledger FOR EACH ROW EXECUTE FUNCTION preserve_ai_budget_ledger();
CREATE TRIGGER preserve_ai_budget_mutation BEFORE UPDATE OR DELETE ON ai_budget_mutations FOR EACH ROW EXECUTE FUNCTION preserve_ai_budget_ledger();
