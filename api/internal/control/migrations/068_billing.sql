-- Opt-in payment receipts. Existing manual grants and request reservations are
-- retained. Payment effects reference the original immutable credit ledger;
-- no browser redirect, invoice creation, or checkout dispatch issues credits.
CREATE TABLE billing_invoices (
 id text PRIMARY KEY CHECK(id ~ '^bil_[a-f0-9]{24}$'),
 org_id text NOT NULL REFERENCES organizations(id),
 actor_id text NOT NULL REFERENCES users(id),
 provider_identity text NOT NULL CHECK(provider_identity ~ '^[a-f0-9]{64}$'),
 provider_account text NOT NULL,
 livemode boolean NOT NULL,
 offer_id text NOT NULL CHECK(offer_id ~ '^offer_[a-f0-9]{24}$'),
 offer_revision text NOT NULL CHECK(offer_revision ~ '^[a-f0-9]{64}$'),
 offer_name text NOT NULL CHECK(length(offer_name) BETWEEN 1 AND 80),
 currency text NOT NULL CHECK(currency='USD'),
 amount_minor bigint NOT NULL CHECK(amount_minor BETWEEN 50 AND 99999999),
 credit_micros bigint NOT NULL CHECK(credit_micros BETWEEN 1 AND 1000000000000),
 binding text NOT NULL CHECK(binding ~ '^[a-f0-9]{64}$'),
 version bigint NOT NULL DEFAULT 1 CHECK(version BETWEEN 1 AND 1000000000000),
 checkout_state text NOT NULL DEFAULT 'open' CHECK(checkout_state IN ('open','dispatching','ready','uncertain','expired')),
	checkout_requested_at timestamptz,
	checkout_dispatched_at timestamptz,
 session_id text,
 intent_id text,
 charge_id text,
 checkout_key_version text,
 checkout_ciphertext bytea,
 checkout_expires_at timestamptz,
 paid boolean NOT NULL DEFAULT false,
 refunded_minor bigint NOT NULL DEFAULT 0 CHECK(refunded_minor>=0 AND refunded_minor<=amount_minor),
 dispute_id text,
 dispute_state text CHECK(dispute_state IN ('needs_response','under_review','won','lost','prevented','warning_needs_response','warning_under_review','warning_closed')),
 dispute_minor bigint NOT NULL DEFAULT 0 CHECK(dispute_minor>=0 AND dispute_minor<=amount_minor),
 applied_credit_micros bigint NOT NULL DEFAULT 0 CHECK(applied_credit_micros>=0 AND applied_credit_micros<=credit_micros),
 processing_token text,
 processing_until timestamptz,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(id,org_id),
 UNIQUE(provider_identity,session_id),
 UNIQUE(provider_identity,intent_id),
 UNIQUE(provider_identity,charge_id),
 CHECK((checkout_key_version IS NULL)=(checkout_ciphertext IS NULL)),
 CHECK(checkout_ciphertext IS NULL OR octet_length(checkout_ciphertext) BETWEEN 28 AND 16384),
 CHECK((processing_token IS NULL)=(processing_until IS NULL)),
 CHECK(paid OR applied_credit_micros=0),
 CHECK(checkout_state<>'ready' OR (session_id IS NOT NULL AND checkout_key_version IS NOT NULL AND checkout_expires_at IS NOT NULL)),
 CHECK(checkout_dispatched_at IS NULL OR checkout_requested_at IS NOT NULL),
 CHECK(NOT paid OR checkout_dispatched_at IS NOT NULL),
 CHECK((dispute_id IS NULL AND dispute_state IS NULL AND dispute_minor=0) OR (dispute_id IS NOT NULL AND dispute_state IS NOT NULL AND dispute_minor>0))
);
CREATE INDEX billing_invoices_org_time ON billing_invoices(org_id,created_at DESC,id DESC);

CREATE TABLE billing_mutations (
 id text PRIMARY KEY CHECK(id ~ '^blm_[a-f0-9]{24}$'),
 actor_id text NOT NULL REFERENCES users(id),
 org_id text NOT NULL REFERENCES organizations(id),
 invoice_id text,
 key_hash text NOT NULL,
 request_hash text NOT NULL,
 response jsonb NOT NULL CHECK(jsonb_typeof(response)='object' AND octet_length(response::text)<=65536),
 http_status integer NOT NULL CHECK(http_status IN (200,201,202)),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(actor_id,key_hash),
 FOREIGN KEY(invoice_id,org_id) REFERENCES billing_invoices(id,org_id)
);

CREATE TABLE billing_inbox (
 id text PRIMARY KEY CHECK(id ~ '^ble_[a-f0-9]{24}$'),
 provider_identity text NOT NULL CHECK(provider_identity ~ '^[a-f0-9]{64}$'),
 event_id text NOT NULL CHECK(length(event_id) BETWEEN 4 AND 255),
 event_type text NOT NULL CHECK(length(event_type) BETWEEN 1 AND 128),
 object_id text NOT NULL CHECK(length(object_id) BETWEEN 3 AND 255),
 api_version text NOT NULL,
 livemode boolean NOT NULL,
 payload_sha256 text NOT NULL CHECK(payload_sha256 ~ '^[a-f0-9]{64}$'),
 source_kind text NOT NULL CHECK(source_kind IN ('signed_webhook','operator_reconcile')),
 source_actor text REFERENCES users(id),
 source_mutation text UNIQUE REFERENCES billing_mutations(id) DEFERRABLE INITIALLY DEFERRED,
 reason text,
 evidence_sha256 text,
 invoice_id text REFERENCES billing_invoices(id),
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','processing','applied','ignored','review')),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 1000),
 next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 lease_token text,
 lease_until timestamptz,
 error_code text,
 observation jsonb NOT NULL DEFAULT '[]'::jsonb CHECK(jsonb_typeof(observation)='array' AND octet_length(observation::text)<=16384),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(provider_identity,event_id),
 UNIQUE(id,invoice_id),
 CHECK((lease_token IS NULL)=(lease_until IS NULL)),
 CHECK((source_kind='signed_webhook' AND source_actor IS NULL AND source_mutation IS NULL AND reason IS NULL AND evidence_sha256 IS NULL)
 OR (source_kind='operator_reconcile' AND source_actor IS NOT NULL AND source_mutation IS NOT NULL AND reason IS NOT NULL AND evidence_sha256 IS NOT NULL AND length(reason) BETWEEN 8 AND 500 AND evidence_sha256 ~ '^[a-f0-9]{64}$'))
);
CREATE INDEX billing_inbox_work ON billing_inbox(state,next_attempt_at,created_at,id);

CREATE TABLE billing_credit_effects (
 id text PRIMARY KEY CHECK(id ~ '^blc_[a-f0-9]{24}$'),
 invoice_id text NOT NULL,
 org_id text NOT NULL,
 inbox_id text NOT NULL,
 credit_entry_id text NOT NULL UNIQUE REFERENCES ai_credit_ledger(id),
 credit_delta bigint NOT NULL CHECK(credit_delta<>0 AND credit_delta BETWEEN -1000000000000 AND 1000000000000),
 applied_before bigint NOT NULL CHECK(applied_before>=0),
 applied_after bigint NOT NULL CHECK(applied_after>=0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(invoice_id,org_id) REFERENCES billing_invoices(id,org_id),
 FOREIGN KEY(inbox_id,invoice_id) REFERENCES billing_inbox(id,invoice_id),
 UNIQUE(invoice_id,inbox_id),
 CHECK(applied_before+credit_delta=applied_after)
);
CREATE TRIGGER billing_mutations_immutable BEFORE UPDATE OR DELETE ON billing_mutations FOR EACH ROW EXECUTE FUNCTION guard_ai_input_receipt();
CREATE TRIGGER billing_credit_effects_immutable BEFORE UPDATE OR DELETE ON billing_credit_effects FOR EACH ROW EXECUTE FUNCTION guard_ai_input_receipt();

CREATE FUNCTION guard_billing_invoice() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'Billing history is retained'; END IF;
 IF (NEW.id,NEW.org_id,NEW.actor_id,NEW.provider_identity,NEW.provider_account,NEW.livemode,NEW.offer_id,NEW.offer_revision,NEW.offer_name,NEW.currency,NEW.amount_minor,NEW.credit_micros,NEW.binding,NEW.created_at)
 IS DISTINCT FROM (OLD.id,OLD.org_id,OLD.actor_id,OLD.provider_identity,OLD.provider_account,OLD.livemode,OLD.offer_id,OLD.offer_revision,OLD.offer_name,OLD.currency,OLD.amount_minor,OLD.credit_micros,OLD.binding,OLD.created_at)
 OR NEW.version<>OLD.version+1 OR NEW.updated_at<OLD.updated_at OR OLD.paid AND NOT NEW.paid OR NEW.refunded_minor<OLD.refunded_minor
 OR OLD.session_id IS NOT NULL AND NEW.session_id IS DISTINCT FROM OLD.session_id
 OR OLD.intent_id IS NOT NULL AND NEW.intent_id IS DISTINCT FROM OLD.intent_id
 OR OLD.charge_id IS NOT NULL AND NEW.charge_id IS DISTINCT FROM OLD.charge_id
 OR OLD.checkout_requested_at IS NOT NULL AND NEW.checkout_requested_at IS DISTINCT FROM OLD.checkout_requested_at
 OR OLD.checkout_dispatched_at IS NOT NULL AND NEW.checkout_dispatched_at IS DISTINCT FROM OLD.checkout_dispatched_at
 THEN RAISE EXCEPTION 'Invalid billing invoice transition'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER billing_invoice_guard BEFORE UPDATE OR DELETE ON billing_invoices FOR EACH ROW EXECUTE FUNCTION guard_billing_invoice();

CREATE FUNCTION guard_billing_inbox() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'Payment event receipt is retained'; END IF;
 IF OLD.state IN ('applied','ignored') AND to_jsonb(NEW) IS DISTINCT FROM to_jsonb(OLD) THEN RAISE EXCEPTION 'Final payment receipt is immutable'; END IF;
 IF (NEW.id,NEW.provider_identity,NEW.event_id,NEW.event_type,NEW.object_id,NEW.api_version,NEW.livemode,NEW.payload_sha256,NEW.source_kind,NEW.source_actor,NEW.source_mutation,NEW.reason,NEW.evidence_sha256,NEW.created_at)
 IS DISTINCT FROM (OLD.id,OLD.provider_identity,OLD.event_id,OLD.event_type,OLD.object_id,OLD.api_version,OLD.livemode,OLD.payload_sha256,OLD.source_kind,OLD.source_actor,OLD.source_mutation,OLD.reason,OLD.evidence_sha256,OLD.created_at)
 OR OLD.invoice_id IS NOT NULL AND NEW.invoice_id IS DISTINCT FROM OLD.invoice_id
 OR NEW.attempts<OLD.attempts OR NEW.updated_at<OLD.updated_at
 THEN RAISE EXCEPTION 'Invalid payment receipt transition'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER billing_inbox_guard BEFORE UPDATE OR DELETE ON billing_inbox FOR EACH ROW EXECUTE FUNCTION guard_billing_inbox();

CREATE FUNCTION guard_billing_credit_effect() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE entry ai_credit_ledger%ROWTYPE; invoice billing_invoices%ROWTYPE;
BEGIN
 SELECT * INTO STRICT entry FROM ai_credit_ledger WHERE id=NEW.credit_entry_id;
 SELECT * INTO STRICT invoice FROM billing_invoices WHERE id=NEW.invoice_id;
 IF entry.org_id<>NEW.org_id OR entry.credit_delta<>NEW.credit_delta OR entry.spent_delta<>0 OR entry.reserved_delta<>0
 OR entry.source_event_id IS DISTINCT FROM 'billing:'||NEW.id OR entry.actor_id IS DISTINCT FROM invoice.actor_id
 OR entry.kind<>(CASE WHEN NEW.credit_delta>0 THEN 'grant' ELSE 'adjustment' END)
 OR NEW.applied_after>invoice.credit_micros OR NEW.applied_before<>invoice.applied_credit_micros
 THEN RAISE EXCEPTION 'Payment credit effect must match its immutable ledger entry'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER billing_credit_effect_guard BEFORE INSERT ON billing_credit_effects FOR EACH ROW EXECUTE FUNCTION guard_billing_credit_effect();

CREATE FUNCTION guard_billing_credit_commit() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE target text; actual bigint; expected bigint; invoice billing_invoices%ROWTYPE; liability bigint; entitlement bigint;
BEGIN
 IF TG_TABLE_NAME='billing_credit_effects' THEN target:=NEW.invoice_id; ELSE target:=NEW.id; END IF;
 SELECT * INTO STRICT invoice FROM billing_invoices WHERE id=target;
 actual:=invoice.applied_credit_micros;
 SELECT COALESCE(sum(credit_delta),0) INTO expected FROM billing_credit_effects WHERE invoice_id=target;
 IF actual IS DISTINCT FROM expected THEN RAISE EXCEPTION 'Payment effects and invoice counters must commit together'; END IF;
 liability:=LEAST(invoice.amount_minor,invoice.refunded_minor+CASE WHEN invoice.dispute_state IN ('needs_response','under_review','lost') THEN invoice.dispute_minor ELSE 0 END);
 entitlement:=0;
 -- Round the cumulative reversal down, retaining any fractional credit until
 -- the final reversal. This is the same exact policy as CreditReversal.
 IF invoice.paid THEN entitlement:=invoice.credit_micros-floor(invoice.credit_micros::numeric*liability/invoice.amount_minor)::bigint; END IF;
 IF actual<>entitlement THEN RAISE EXCEPTION 'Verified payment liability and credited entitlement must commit together'; END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER billing_invoice_credit_commit AFTER INSERT OR UPDATE ON billing_invoices DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION guard_billing_credit_commit();
CREATE CONSTRAINT TRIGGER billing_effect_credit_commit AFTER INSERT ON billing_credit_effects DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION guard_billing_credit_commit();
