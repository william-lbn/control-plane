# Functions: explicit retained-version wake

> 历史增量合同：公开 HTTP/SSE、自动休眠/冷醒已在后续 0.1.28 独立验收；本文旧增量的排除范围不代表最新运行状态。最新范围以 [PRODUCTION-GATES](PRODUCTION-GATES.md) 与对应版本交付为准。

## 1. Product contract and official direction

The current Neon documentation describes branch-scoped Node.js Functions next
to Postgres, retained deployments and idle eviction. This slice implements an
explicit operator wake through our Go API and React Console; it is an extension
of our preview API, not a claim that Neon exposes this exact route or that
automatic scale to zero, HTTP/SSE/WebSocket ingress or triggers are implemented.
References: [official overview](https://neon.com/docs/compute/functions/overview),
[runtime limits](https://neon.com/docs/compute/functions/reference/runtime-limits)
and local website `content/docs/compute/functions/overview.md` (updated 2026-09-22).

The user can stop a Function and click **唤醒保留版本** without uploading code or
re-entering secrets. A new boot uses the last successful immutable source and
environment and the original restricted SQL identity. In-process RAM, timers,
pending calls and background work are not restored. The action does not execute
an application invocation; subsequent **调用函数** remains a separate request.

## 2. API, data and reconciliation

`POST /api/v1/projects/{project}/branches/{branch}/functions/{slug}/resume`

| Input/result | Contract |
| --- | --- |
| Authorization | Effective project Editor/Admin, session and CSRF |
| Headers | `Idempotency-Key`; `If-Match: "<decimal version_token>"` |
| Body | Exactly `{}`; null/code/env/other fields return 422 |
| 202 | Existing Function ID, durable Operation and Location |
| 404 | Unauthorized scope or missing Function; no ID oracle |
| 409 | Not fully retired, not suspended, no successful source, key conflict or competing Operation |
| 412 / 428 | Stale / missing or invalid numeric version |
| 503 | Metadata, trust/backing or disabled Driver unavailable |

The durable model reuses `function_definitions`, `function_deployments`,
`function_deployment_backings`, `function_instances`, SQL credentials,
`operations`, `operation_steps`, idempotency and audit tables. There is no new
metadata migration. Wake admits a new deployment and execution generation, not
a new Function or SQL role. The Worker Operation remains `deploy_function`;
transactional audit action `resume_function` distinguishes the user intent.

```mermaid
sequenceDiagram
    participant U as React Console
    participant A as Go API
    participant M as PostgreSQL metadata
    participant B as Immutable backing
    participant W as Go Worker
    participant G as New guest
    U->>A: POST resume + key + current version + {}
    A->>M: Resolve scope / original intent or replay
    A->>B: Verify retained env UID/MAC and bundle digest
    A->>M: Lock parents and definition; recheck retirement/version/source
    A->>M: Commit deployment, backing, generation, Operation, audit
    A-->>U: 202 original durable Operation
    W->>M: Lease existing deployment pipeline
    W->>G: New candidate and authenticated boot
    G-->>W: TLS/HMAC original readiness
    W->>M: Publish only matching current generation
    U->>A: Explicit runtime observation / separate invocation
```

Admission rejects active, deleting, unretired or never-successful definitions
before backing writes, then rechecks eligibility and pending branch Operations
under the original transaction locks. The request fingerprint has a separate
wake domain, includes scope and expected version, and stores no secrets. Replays
return the original Operation before current-state checks; conflicting reuse on
the deployment route returns 409. A lost response is resolved with the same key
or original Operation, never blind submission with a fresh key. If the new boot
fails, retain its evidence and explicitly stop/retire it before recovery.

## 3. UI and focused Linux acceptance

The wake button is available only to editors with a suspended definition and
an existing successful deployment pointer; authoritative API checks remain
mandatory. It submits no upload/env editor contents, follows the original
Operation and refreshes metadata after success. Observation stays explicit.

`web/e2e/functions-wake.spec.ts` uses the protected retained owner fixture and
pinned example ZIP on Linux. It creates one owned Function, records its original
source/boot, stops it and the Writer, then wakes from the UI. It verifies a new
boot/deployment/generation, unchanged bundle and environment names, unchanged
actual env value despite unsaved UI edits, same-key replay, cross-route key
conflict and stale version rejection. One real Proxy/Neon SQL read proves the
same restricted role and retained data. Finally it normally stops/deletes that
Function and suspends Compute; data and all receipts remain.

Use the trusted Linux workflow's `functions-wake.spec.ts` choice. Set the private
`NEON_E2E_FUNCTIONS_MONITOR_FIXTURE` Linux owner fixture path documented by the
observation slice and the protected Console password. The workflow packages the
sample outside Git. No Python runtime is part of the product.

**New wake live acceptance passed at exact runtime/test source `0198f7c` / Helm
0.1.21.** Linux Job `publication-ui-20261012113626` completed at 2026-10-12 19:38
Asia/Shanghai, with the new boot, retained source/env/SQL identity, replay/conflict/
stale-version checks and normal retirement/Compute zero. Public Linux CI
`38223473074` passed all 13 jobs and 723 Go/PostgreSQL checks, zero failures/skips.
See the [full immutable acceptance receipt](https://github.com/william-lbn/neon-helm/blob/v0.1.21/docs/ACCEPTANCE-2026-10-12-FUNCTIONS-WAKE.md).
Monitoring acceptance at runtime `be557e0` / Helm 0.1.20 passed by original stopped
checkpoint continuation; that result does not cover this new wake endpoint.

## 4. Independent next gates

Automatic eviction/cold wake needs declared background-work semantics, durable
drain/intent fencing and actual request admission. Monitoring counters alone
cannot authorize retirement. Full public streaming ingress, triggers, branch
inheritance, logs/metering, issuer rotation, multi-instance fencing and HA/DR
remain separate implementation and acceptance work.
