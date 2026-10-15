# Personal saved SQL queries

The Console can save, load, rename/update and delete personal SQL drafts. Loading
only changes editor text, clears the connection password/results and selects
script mode; it never executes SQL or wakes Compute. This slice does not implement
SQL result storage or AI-assisted SQL. Optional personal execution history is now accepted separately in [SQL-QUERY-HISTORY](SQL-QUERY-HISTORY.md). Explicit author-controlled project sharing is now specified and accepted separately in [SHARED-QUERIES](SHARED-QUERIES.md).

## Model and interfaces

Migration 027: `saved_queries` binds ID/project/owner, name, generation, key version,
encrypted SQL, create/update timestamps and deletion tombstone. Names are metadata;
SQL is AES-GCM encrypted using a purpose-derived versioned Backend key. AAD binds
project, owner, ID and content generation. Retain old keyring versions when rotating
until the corresponding records are reencrypted/expired. No password/results fields.
Deletion hides a tombstone; physical retention/erasure remains the lifecycle gate.

`saved_query_requests` stores actor/key hash, request hash and metadata-only reply.
Durable idempotency uses the fixed idempotency key, independent of active encryption
version. Mutation, replay and audit commit in one PostgreSQL transaction. Replays do
not duplicate records/audit or retain plaintext SQL in metadata JSON.

| Method / route | Contract |
| --- | --- |
| GET `/api/v1/projects/{project}/saved-queries` | Personal metadata list, <=50; current Editor+ |
| POST same | `{name,sql}`; Idempotency-Key; encrypted draft, metadata reply |
| GET `.../{query}` | Owner only, decrypted SQL and ETag; no-store |
| PATCH `.../{query}` | Full `{name,sql}` replacement; If-Match + Idempotency-Key |
| DELETE `.../{query}` | Owner tombstone; If-Match + Idempotency-Key |

Other owners receive 404 even if they administer the same project; Viewer cannot
read personal SQL. API keys remain intersected with issuer/project permissions.
Request limit: name 1–100 UTF-8 bytes, SQL <=64 KiB, 50 live drafts per user/project.
Unsupported password/result fields are rejected. Updating a stale version gives
412; reusing a key for different input gives 409. Plain SQL is returned only by an
authorized detail read; never by list, mutation replay or audit.

## UI and verification

SQL workbench → enter text → **保存查询** → name → **另存为新查询**.
Choose a saved query and **加载到编辑器**. Edit name/text and explicitly update;
delete confirms that database data is unaffected. All these actions work with a
suspended Endpoint and an empty connection password. Do not press Execute when
testing metadata-only storage.

Go/real PostgreSQL gate: encryption binding/tamper/retained-key rotation; durable
creation/update/delete replay, stale versions, peer/tenant/Viewer denial, no SQL in
ciphertext/replay JSON, one audit per mutation. Live Linux UI gate additionally
checks exact Unicode/newline roundtrip after reload, manual update/delete and zero
Compute workload creation. Failures and owned tombstones remain recorded.
