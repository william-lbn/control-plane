# Managed database Owner transfer

## 1. Scope

This implements the native PostgreSQL `ALTER DATABASE ... OWNER TO ...` product
path through the Console and Go control plane. The [official database guide](https://neon.com/docs/manage/databases)
and [PostgreSQL owner semantics](https://www.postgresql.org/docs/16/sql-alterdatabase.html)
are the reference; our `/api/v1` contract is not claimed to mirror Neon `/api/v2`.

The first slice transfers a ready API-managed non-system database between ready
API-managed non-system roles on the same branch. It retains the original database
OID and data, updates real SQL and every Writer/Reader cold configuration, and
commits metadata only after verification. System/default-owner databases, rename,
arbitrary external DDL adoption and full distributed fencing remain separate.
Database ownership changes do not reassign existing table/schema ownership or
ACLs. PostgreSQL enforces the existing service role privileges; no temporary
grants, elevated credentials or session termination are added.

## 2. API and durable model

`PATCH /api/v1/projects/{project}/branches/{branch}/databases/{database}`

| Field | Contract |
| --- | --- |
| Authorization | Effective project Editor/Admin and session CSRF |
| Headers | `Idempotency-Key`, `If-Match: "<version_token>"` |
| Body | `{ "owner": "managed_new_owner" }`; unknown fields rejected |
| 202 | Original durable branch catalog Operation |
| 404 / 403 | Hidden unauthorized scope / insufficient effective permission |
| 409 | Unmanaged/foreign object, target not ready, competing Operation, key conflict |
| 412 / 428 | Stale / absent-invalid expected version |
| 422 / 503 | Invalid/protected/unchanged target / unavailable metadata or SQL |

Catalog listings add a decimal-string `version_token` to API-managed entries.
Forward-only migration 024 permits the `updating` database state. No new table
is introduced. Payload contains project/branch/Writer/database, original OID,
previous/final owner and expected version, without passwords. Credentials retain
their existing Secret references.

```mermaid
sequenceDiagram
    participant U as Console
    participant A as Go API
    participant M as Metadata
    participant P as Neon Proxy / PostgreSQL
    participant W as Worker
    participant K as Compute configuration
    U->>A: PATCH owner + key + string version
    A->>M: Authorize / replay / metadata eligibility
    A->>P: Observe original OID, owner and marker
    A->>M: Lock parents/branch; recheck roles/version/Operations
    A->>M: Commit updating + Operation + steps + audit + idempotency
    W->>P: Verify original identity and both role markers; ALTER OWNER
    W->>K: CAS all Compute specs, verify original Proxy routes
    W->>P: Reobserve original OID and final owner
    W->>M: Leased expected-version commit; ready + new owner
    U->>A: Observe original Operation / refreshed catalog
```

Same-key replay returns the original Operation even after completion. Different
body/version under that key is rejected. Stale/unmanaged requests stop before
SQL and do not wake Compute. Unknown SQL acknowledgement is resolved against the
captured OID/marker and original-or-final owner. Metadata acknowledgement replay
requires exactly the expected final owner/version, never a later version. Normal
Worker step/lease guards remain; they do not prove external distributed fencing.

## 3. Console and acceptance

The new owner form offers only eligible managed objects and roles, uses the
catalog string version, follows the original Operation and clears only submitted
values. Scope epochs prevent late responses from another project/branch changing
the currently displayed catalog. Server authorization remains authoritative.

`database_owner_integration_test.go` tests actual non-superuser PostgreSQL owner
transfer, same-OID replay, foreign OID/marker/owner rejection and metadata CAS/
lost-acknowledgement replay. The disposable `control_ci` gate is separate from
actual Neon acceptance. API authorization/stale/protected/unmanaged negative
checks must finish before external SQL.

The protected Linux `database-owner.spec.ts` suite reuses an explicit owned
project/branch/Writer, creates two managed roles and one retained database through
the UI, writes under the old owner, changes owner through the UI and verifies
replay/conflict/stale requests. It connects with both actual passwords through
the UI SQL workbench, confirms new-owner CREATE and preserved object ACLs/data,
then checks Writer cold wake and a newly created Reader plus Reader cold wake.
Only the owned test Reader and running Compute are retired. The database, roles,
password fixtures and all evidence are retained for reproduction.

Configure `NEON_E2E_CATALOG_OWNER_FIXTURE` as a protected Linux file containing
`project_id`, `branch_id` and `writer_id`, and the protected Console password.
The UI test writes its role credentials/recovery fixture outside public Git and
reports no credentials. A failed accepted Operation is inspected or continued
with its original identity; do not rerun creation blindly.

**Owner-transfer live acceptance passed by original-fixture continuation at
runtime `e2dc587` / unified Helm 0.1.22.** Actual SQL ownership/data/ACLs, replay/
conflict/stale checks, Writer and Reader cold wake, original credentials and
normal resource retirement passed. Final Linux Job `publication-ui-20261012130243`
completed on 2026-10-12 at 21:03 Asia/Shanghai; no managed Compute VM or Runner
remained. Public CI `38246909591`: 13 successful jobs, 731 Go/PostgreSQL passing
checks, zero failures/skips. See the [exact receipt, original test-only corrections
and limits](https://github.com/william-lbn/neon-helm/blob/v0.1.22/docs/ACCEPTANCE-2026-10-12-DATABASE-OWNER.md).

Neon Console/API roles inherit `pg_read_all_data` and `pg_write_all_data`; they
are administrative roles. The test compares actual pre/post object ownership
and ACLs, without inventing a least-privilege denial or reducing existing grants.
The current single-statement workbench rejects multi-statement prepared queries;
each setup statement is submitted separately. Script-mode support is a separate
next product slice. Functions receipts remain pinned to their own releases.
