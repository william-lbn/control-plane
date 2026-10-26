# Functions Edge live probe

This SQL-free fixture has no package download, migration or deployment authority.
For an explicitly owned A/B deployment, copy `handler.mjs` and the matching entry
as `index.mjs` into a new private directory. Use the existing Linux
`tools/package-function-example.mjs` packager. The Guest must provide its reviewed
WebSocket helper. Restore the original deployment and public policy afterwards.

Run `node tools/functions-edge-probe/probe.mjs INPUT.json CHECKPOINT.json` with
cached Linux Node 24. Private input fields are `project`, `branch`, `function_id`,
`slug`, `deployment_id`, optional `boot_id`, `version` A/B, a unique `stage`, exact
`public_url`, `console_origin` and `console_headers` (Cookie or Authorization).
Keep this input outside source. Use verified HTTPS; `lab_http: true` permits only
literal private/loopback IPv4. The checkpoint directory must have mode 0700.
Console credentials go only to the configured Console origin.

SSE emits the second chunk only after a separate release request. Four streams
and ten-second expiry bound the fixture. A fixed 50,000-iteration CPU call provides
a small resource observation; it is not a performance benchmark. The client checks
deployment ownership, trusted receipts, WS directions and actual cgroup counters.
Unavailable samples fail. Completed steps are reused; pending invocation steps
require review of their original receipt before another attempt. Read-only usage
collection may resume. The client neither logs in nor changes policy or code.

Run A, finish streams, stop and confirm retirement, then B and rollback A as
separate stages under the existing maintenance contract. No hot replacement is
claimed. Fixture self-checks run with
`node --test tools/functions-edge-probe/fixture.test.mjs`; these are not real
Edge/Guest acceptance and must be recorded separately.
