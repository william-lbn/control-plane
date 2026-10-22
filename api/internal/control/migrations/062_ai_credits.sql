-- Opt-in self-hosted operator credits. No payment processor, cash deposit, or
-- transferable money is inferred from an operator grant. History is retained.
CREATE TABLE ai_credit_accounts (
 org_id text PRIMARY KEY REFERENCES organizations(id),
 version bigint NOT NULL CHECK(version BETWEEN 1 AND 1000000000000),
 enabled boolean NOT NULL DEFAULT false,
 currency text NOT NULL DEFAULT 'USD' CHECK(currency='USD'),
 credited_micros bigint NOT NULL DEFAULT 0 CHECK(credited_micros BETWEEN -1000000000000000 AND 1000000000000000),
 spent_micros bigint NOT NULL DEFAULT 0 CHECK(spent_micros>=0),
 reserved_micros bigint NOT NULL DEFAULT 0 CHECK(reserved_micros>=0),
 updated_by text NOT NULL REFERENCES users(id),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE UNIQUE INDEX ai_gateway_requests_credit_scope ON ai_gateway_requests(id,project_id,branch_id);
CREATE TABLE ai_credit_reservations (
 request_id text PRIMARY KEY,
 org_id text NOT NULL REFERENCES ai_credit_accounts(org_id),
 project_id text NOT NULL,
 branch_id text NOT NULL,
 price jsonb NOT NULL CHECK(jsonb_typeof(price)='object'),
 reserved_micros bigint NOT NULL CHECK(reserved_micros>=0),
 state text NOT NULL CHECK(state IN ('reserved','uncertain','settled','released')),
 actual_micros bigint CHECK(actual_micros>=0),
 usage jsonb,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 settled_at timestamptz,
 UNIQUE(request_id,org_id),
 FOREIGN KEY(project_id,org_id) REFERENCES projects(id,org_id),
 FOREIGN KEY(request_id,project_id,branch_id) REFERENCES ai_gateway_requests(id,project_id,branch_id),
 CHECK((state IN ('settled','released'))=(actual_micros IS NOT NULL)),
 CHECK(state<>'released' OR actual_micros=0)
);
CREATE INDEX ai_credit_reservations_org ON ai_credit_reservations(org_id,state,created_at);
CREATE TABLE ai_credit_ledger (
 id text PRIMARY KEY,
 org_id text NOT NULL REFERENCES ai_credit_accounts(org_id),
 request_id text,
 kind text NOT NULL CHECK(kind IN ('grant','adjustment','reserve','settle','release','uncertain','reconcile')),
 credit_delta bigint NOT NULL DEFAULT 0,
 spent_delta bigint NOT NULL DEFAULT 0,
 reserved_delta bigint NOT NULL DEFAULT 0,
 source_event_id text,
 reason text,
 actor_id text REFERENCES users(id),
 evidence_sha256 text CHECK(evidence_sha256 ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(request_id,org_id) REFERENCES ai_credit_reservations(request_id,org_id),
 UNIQUE(request_id,kind),
 UNIQUE(org_id,source_event_id),
 CHECK((kind IN ('grant','adjustment'))=(source_event_id IS NOT NULL)),
 CHECK(kind NOT IN ('grant','adjustment') OR (request_id IS NULL AND actor_id IS NOT NULL AND evidence_sha256 IS NOT NULL AND reason IS NOT NULL AND credit_delta<>0 AND spent_delta=0 AND reserved_delta=0)),
 CHECK(kind<>'grant' OR credit_delta>0),
 CHECK(kind IN ('grant','adjustment') OR (request_id IS NOT NULL AND credit_delta=0))
);
CREATE INDEX ai_credit_ledger_org_time ON ai_credit_ledger(org_id,created_at DESC,id DESC);
CREATE TABLE ai_credit_mutations (
 actor_id text NOT NULL REFERENCES users(id),
 org_id text NOT NULL REFERENCES organizations(id),
 key_hash text NOT NULL,
 request_hash text NOT NULL,
 response jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(actor_id,key_hash)
);
CREATE TRIGGER preserve_ai_credit_ledger BEFORE UPDATE OR DELETE ON ai_credit_ledger
 FOR EACH ROW EXECUTE FUNCTION preserve_ai_budget_ledger();
CREATE TRIGGER preserve_ai_credit_mutations BEFORE UPDATE OR DELETE ON ai_credit_mutations
 FOR EACH ROW EXECUTE FUNCTION preserve_ai_budget_ledger();
