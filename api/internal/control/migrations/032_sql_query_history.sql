-- Personal execution history: ciphertext only; never connection passwords/results.
CREATE TABLE sql_query_history (
 id text PRIMARY KEY,
 project_id text NOT NULL REFERENCES projects(id),
 owner_id text NOT NULL REFERENCES users(id),
 endpoint_id text NOT NULL REFERENCES endpoints(id),
 branch_id text NOT NULL,
 mode text NOT NULL CHECK(mode IN ('single','script')),
 database_name text NOT NULL, role_name text NOT NULL,
 key_version text NOT NULL, sealed_sql bytea NOT NULL,
 original_bytes integer NOT NULL CHECK(original_bytes BETWEEN 1 AND 32000),
 truncated boolean NOT NULL,
 outcome text NOT NULL CHECK(outcome IN ('admitted','completed','failed','not_submitted','outcome_unknown')),
 sqlstate text NOT NULL DEFAULT '' CHECK(sqlstate='' OR sqlstate ~ '^[A-Z0-9]{5}$'),
 started_at timestamptz NOT NULL DEFAULT now(),
 deadline_at timestamptz NOT NULL,
 finished_at timestamptz, duration_ms bigint CHECK(duration_ms>=0),
 FOREIGN KEY(endpoint_id,project_id,branch_id) REFERENCES endpoints(id,project_id,branch_id)
);
CREATE INDEX sql_query_history_owner_page ON sql_query_history(project_id,owner_id,started_at DESC,id DESC);
