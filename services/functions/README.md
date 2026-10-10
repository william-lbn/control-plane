# Isolated Functions runtime foundations

The first source increment contains the Node.js 24 Fetch runtime and Go bundle,
manager-authentication, boot-scoped HTTP/SSE proxy, immutable installation and
guest-boundary primitives. It is not an enabled
Functions product. It still requires the immutable VM image, supervisor,
metadata/branch Driver, API, Console and actual microVM acceptance.

The runtime accepts an ESM module exporting a `fetch(Request): Response` object
or a bare asynchronous function. HTTP bodies, request concurrency and background
registration are bounded. SSE forwards each chunk without application buffering.
The self-hosted `waitUntil` helper retains pending work after the response. This
increment does not claim proprietary Neon SDK compatibility, WebSockets,
triggers, branching or authenticated public invocation.

Go validation rejects unsafe ZIP paths, symlinks, ambiguous entries, expanded
size bombs and reserved environment overrides. Manager HMAC signs the exact
method, URI, body digest, time and nonce; concurrent replay fails closed. Guest
primitives enforce a dedicated cgroup, an unprivileged no-new-privileges child,
explicit SQL/DNS egress and private-range rejection for optional public HTTPS.
The own-fork guest kernel lacks IPv6 netfilter, so guest IPv6 must be disabled.
These primitives need actual guest negative tests before an isolation claim.

The accepted Linux foundation run is `functions-foundation-20261010203931`:
49 Go checks (race/vet, zero failures/skips) and 9 actual Node HTTP/SSE checks.
The original failed ZIP corruption sample is retained separately. This evidence
does not include a guest boot, native Driver, Console or product zero/wake.

Run the foundation checks on Linux with the repository's locked toolchains:

```bash
cd api
go test -race ./internal/functions
cd ../services/functions
npm run check
npm test
```

The current wrapper uses an absolute 15-minute execution budget. Matching the
hosted service's separate response/stream/background budgets is a later gate.
The privileged supervisor must independently enforce deadlines/cgroup cleanup;
untrusted Node counters are not an external fence. Customer bundles and
environment values are not diagnostic log fields. Do not expose this loopback
runtime as an unauthenticated Kubernetes Service.

Primary design references checked on 2026-10-11 (Asia/Shanghai):

- [Functions overview](https://neon.com/docs/compute/functions/overview)
- [Runtime limits](https://neon.com/docs/compute/functions/reference/runtime-limits)
- [Deploy](https://neon.com/docs/compute/functions/deploy)
