# Recovering Console Operation observation

## Problem and contract

The 2026-10-07 Linux UI acceptance exposed a real failure: Kubernetes/etcd
became unavailable, RKE2 lost its leadership lease, and the project Operation
later succeeded. A transient failed GET stopped the old creation modal's
observation. Backend success and unavailable frontend observation are distinct.

Project/branch/Endpoint creation and Data API enable/disable now share a bounded
read controller. It follows **the same accepted Operation ID**, never resubmits
the creating POST/DELETE and never manufactures backend failed/cancelled state.
No API route/model changes are required; OpenAPI and server states remain authoritative.

```mermaid
sequenceDiagram
  participant UI as Console
  participant API as Go API
  participant W as Worker / metadata
  UI->>API: mutation + Idempotency-Key
  API-->>UI: 202 / resource and Operation ID
  API->>W: durable accepted intent
  UI->>API: GET same Operation
  API-->>UI: transient 503 / network unavailable
  Note over UI: keep ID and last observed state; show observation warning
  W->>W: finish/recover the accepted Operation
  UI->>API: bounded GET retry, same ID
  API-->>UI: succeeded + steps
  UI->>UI: show actual result
```

## Safety and UX

- Retry only observation of an accepted Operation: network failure, request
  timeout, or HTTP 429/500/502/503/504. Use 2/4/8-second bounded backoff and
  honor Retry-After within the remaining observation deadline.
- Stop immediately on 401/403/404/409/422, invalid resource ID or unknown state.
  Permission loss must not be hidden by automatic retry.
- Each read has a ten-second limit; a follow window is eight minutes. After that,
  keep the accepted ID and offer **继续查询操作** or the Operation history.
- `cancelled` is terminal, distinct from transport error. Failed Operation retry
  remains an explicit server action for the same ID, guarded by `retryable`.
- Changing a branch or unmounting aborts frontend reads; it does not cancel the
  durable backend operation or apply old branch state to the new selection.
- While an accepted Data API operation is unresolved, disable new lifecycle
  mutations; continue observation rather than admitting a competing intent.
- Warning text contains no provider credentials, passwords, cookies or query body.

## Verification

`web/tests/follow-operation.test.ts` covers transient recovery, permission loss,
Retry-After, observation deadline, terminal cancellation, wrong identity/state,
unmount and read timeout. Run via Linux `npm test` and the normal TypeScript build.

Set `NEON_E2E_POLL_FAULT=true` for `native-product.spec.ts` and `data-api.spec.ts`.
Linux Chromium injects two failed **observation** responses while the actual
Neon/Go/PG resources continue running. Tests require completion with one UI
creation/enable mutation. The native test can also use the independent Worker
fault fixture. This is a product recovery test, not a full HA/DR certificate.

Keep the original failed attempt, accepted Operation, dump/Pod/Job evidence and
password fixture. Stop its test Compute through the control API when the test
is terminated. Use fresh attempt directories for subsequent acceptance.

## Infrastructure boundary

The incident has RKE2 lease-timeout/restart evidence and I/O wait; no OOM was
observed in the sampled host/nodes. Sharing one physical disk remains a common
failure domain. The initiating storage/host stall is not fully attributed by
these samples. More resource cleanup or a successful UI retry does not qualify
etcd latency, trusted TLS, cross-instance fencing or HA/DR. Preserve the independent
requirements in PRODUCTION-GATES.md; perform builds/pulls and real Compute tests
serially, reserve foundational resources and measure disk/etcd latency.
