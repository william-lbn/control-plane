# Auth local email delivery: accepted 2026-10-14

## Delivery summary

Runtime source **374a38b0f1e3b91456c8a9313a19ad3e2921feb3**, OpenAPI **0.22.0**; unified Helm **0.1.30**, final SMTP chart source **1662b6b455fc5d1b1271baae12a78112b96d1044**. Test-only continuation source **24000093e6b70ebe220f60c855eea9bd26ce9b96** changes only `web/e2e/auth-mail.spec.ts`. Source, rendered manifests, image digests, operations and original account identity are independently recorded.

This is local feature acceptance, not production certification or full Neon SaaS parity. Public SMTP delivery, OAuth/SSO/MFA, HA/DR, immediate offline JWT revocation and end-to-end trusted TLS remain independent gates. Email OTP work is subsequent and is not included in the accepted runtime.

## Verified implementation

- Immutable operator SMTP Secret, read-only only in the branch Auth runtime; separate authenticated inbox reader Secret.
- Optional branch required-email policy; registration cannot fabricate a session before verification.
- Actual SMTP submission to a protected, internal, persistent Mailpit receiver.
- Actual message link verifies the same PostgreSQL application identity; subsequent UI login confirms `emailVerified`.
- One-time 15-minute password recovery; old password denied and new password accepted.
- Actual PostgreSQL tests verify expired/reused reset tokens, unknown-account behavior and server-session revocation. Already issued offline JWTs retain their bounded five-minute lifetime.
- Actual automatic signup delivery and verified login for a distinct controlled account.
- Normal Auth retirement, owned Compute 0, accounts/mail/PVC/Secrets/operations/evidence retained.

## Evidence and exact scope

| Gate | Result / immutable input |
|---|---|
| Public Linux CI | Run **38563260230**, all 14 jobs succeeded |
| Go / real PostgreSQL CI | **790 pass, 0 fail, 0 skip** |
| Local Auth library, real PG/SMTP/TLS | **9 Auth checks + 1 SQL TLS check**, zero skips; includes rejecting non-STARTTLS SMTP in real transport mode |
| Local focused Go | **25 pass, 0 fail, 0 skip**, mail policy, callback trust, cloning and OpenAPI |
| TypeScript | Types, 19 existing web checks and production build succeeded; continuation types/format checked separately |
| Unified Helm | **15 tools, 10 charts, 34 negative cases**; immutable Git archives checked |
| Registry / nodes | Eight matching public own-registry image receipts; **24 serial Linux node imports** |
| Stack | Eight stages succeeded; original full values preserved; PVC/Secrets/manifests audited |
| Initial UI | Job **publication-ui-20261014125012**: first three checks passed; actual SMTP greeting then timed out |
| Same-account UI continuation | Job **publication-ui-20261014132115**: **10 total checks** including retained first three, original recovery, automatic signup delivery and normal cleanup |
| Final audit | Eight releases, exact manifest match, retained identities and Compute 0 |

Historical Functions, SQL, Data API, S3 and inference receipts keep their own versions; this slice does not relabel all previous suites as current full regression.

Private evidence directories: `20261014-auth-mail/{tls-and-policy-attempt1,final-callback-go-attempt1,public-git-chart-030-smtp-fix-attempt1,public-git-unified-030-deploy-attempt1,mailbox-rdns-repair-attempt1,ui-mail-attempt1,ui-original-mail-recovery-attempt1}` and the `auth-mail-374a38b-release-*` image/CI records. Actual mail bodies and application passwords remain in protected Linux fixture files, never public artifacts.

## Failure diagnosis and precise correction

The initial account was actually created but remained unverified. DNS lookup and TCP connection from its real Auth Pod succeeded; the SMTP welcome message did not arrive, and Nodemailer reported a connection greeting timeout. Receiver ingress matched the real Auth labels. The Mailpit documentation confirms SMTP reverse DNS is enabled by default. The receiver correctly had denied egress, so reverse lookup delayed its greeting.

Added `MP_SMTP_DISABLE_RDNS=true` to the optional receiver and positive Helm gates. A preflight compared old and desired manifests: **only Deployment/neon-control-auth-mailbox** changed, and the only template change was that environment entry. A standard Helm upgrade retained the receiver PVC UID and all API/Worker/Web/Auth Pod UIDs. Real SMTP immediately succeeded afterward. Network isolation and authentication stayed in place. No Neon/Postgres/autoscaling source change was needed.

The original account `GZ1F3ucLIY3QxZncLtS8eymOCy8rcqhx` and initial Operation `op_75dfbeb4333e6e220d4d9b5f` were retained. The first test had kept its random password only in memory, so the continuation recovered that same account through an actual reset message, established a new baseline password, then independently tested old/new password behavior with another actual reset. New tests persist continuation inputs in mode-0600 protected files before mutation. This recovery is explicit; it is not an automatic replay of an uncertain mutation.

A second, distinct controlled account verifies automatic signup mail after the configuration repair. Both account rows and all mail remain. Final normal disable Operation: `op_61540d238910cad8f7a45c83`.

## Reproduction and operating instructions

Follow [AUTH-EMAIL-RECOVERY](AUTH-EMAIL-RECOVERY.md) for configuration, data model, exact raw routes, sequence diagrams and Linux UI inputs. Use external immutable operator Secrets. Enable the local receiver only with explicit acknowledgment; its image is pinned, inbox is authenticated/internal, PVC retained and egress denied. Reverse DNS, remote checks and version checks are disabled.

Before application tests after node restart, run [the CNI read-only diagnostic](DELIVERY-2026-10-14-ENVIRONMENT-RECOVERY.md). This incident independently found a segfaulting loopback and four other failing helpers; 63 host checks and authenticated project access passed after precise repairs. Node Ready alone is insufficient.

Run only one protected Linux UI suite at a time. `auth-mail.spec.ts` takes the existing owner database fixture and inbox fixture. For a new acceptance cycle the suite creates controlled application users, verifies actual SMTP delivery and retires its owned Auth/Compute. Preserve failed fixtures; continue them with an explicitly validated checkpoint. Do not erase accounts, messages, storage or logs to make the next run pass.

The developer callback UI is a workbench for application identities. Applications use the same raw branch API and their own exact trusted callback origin; application users do not need a Console administrator account to consume the raw verification/reset API. Console identity registration/recovery is a separate boundary.

## Next local phase

Email OTP: passwordless login/signup, email verification codes, password recovery codes, hashed durable records, expiry/attempt/replay controls, UI, real SMTP and PostgreSQL acceptance. Magic Link and remaining Auth administration follow, then remaining S3/Functions and platform gates. External provider inputs stay deferred; the protected local receiver supports ongoing engineering.
