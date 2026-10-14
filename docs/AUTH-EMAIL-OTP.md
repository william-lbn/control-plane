# Branch Email OTP

## Status and scope

This is the next candidate after accepted local email links/password recovery (runtime 374a38b / Helm 0.1.30). It implements native passwordless signup/login, email verification codes and password recovery codes using pinned Better Auth 1.7.7, PostgreSQL verification records and the actual configured SMTP sender. Real library/PG/SMTP checks pass; deployment and actual Linux UI acceptance remain required. Do not treat this candidate as available in the previously deployed 0.22 runtime.

## Configuration and data model

`ManagedAuthSpec.email_otp_enabled` is optional, defaults false, and is omitted from canonical false requests to preserve existing durable request hashes. True requires operator SMTP configuration. Policy is branch-scoped and immutable for a runtime generation: disable and enable through durable Operations to change it. Cloned/recreated generations clear copied sessions/JWKS/rate-limit/verification records; same-generation retries retain pending records. Existing account rows remain.

No hand-built authentication protocol or second password store is added. Maintained Better Auth generates six-digit codes, stores them **hashed** in the existing branch `neon_auth.verification` table, expires them after 300 seconds and invalidates them after three incorrect verification attempts. Database-backed rate limits additionally bound requests. A successful consume cannot be replayed; verification, sign-in and recovery purposes are distinct.

Email OTP login permits creation of a new application user after mailbox ownership is proved. This branch policy is opt-in. A pre-existing unverified password account is handled by the maintained library's ownership guard: OTP sign-in verifies ownership and retires the old password/sessions. Applications must offer password recovery afterward if they need password login. The Console administrator/organization boundary is independent.

Normal signup still uses the existing email-link policy. Explicit OTP email verification is available through the API/UI; a separate automatic signup verification-method selector is a future extension. Enabling OTP does not claim OAuth, MFA or public SMTP deliverability.

## Component boundary and API

```mermaid
sequenceDiagram
  participant UI as Application / Console workbench
  participant G as Go relay
  participant A as Owned branch Auth
  participant P as Branch PostgreSQL
  participant M as SMTP inbox
  UI->>G: send-verification-otp(email,type)
  G->>G: live branch and opted-in policy
  G->>A: bounded request, trusted client metadata
  A->>P: hashed code + expiry / attempt state
  A->>M: actual code message
  UI->>G: sign-in/email-otp(email,otp)
  G->>A: allowlisted operation
  A->>P: validate and consume; create/verify user and session
  A-->>UI: actual user + application cookie
```

The separate OpenAPI 3.1 contract is `contracts/application-auth-v1.json`, served at `/api/application-auth-openapi.json`; `/api/docs?schema=auth&branch_id=<branch>` selects it and sets the branch server/cookie name. Documentation lookup never wakes Compute or asserts runtime readiness.

Conditional POST actions under `/auth/v1/{branch_id}`:

| Action | Body | Effect |
|---|---|---|
| `email-otp/send-verification-otp` | email, type=`sign-in` or `email-verification` | sends actual code |
| `sign-in/email-otp` | email, otp; optional native SDK name/image | signup/login after ownership proof |
| `email-otp/verify-email` | email, otp | validates actual account email |
| `email-otp/check-verification-otp` | email, type, otp | validates without completing authentication; counts failed attempts |
| `email-otp/request-password-reset` | email | sends recovery code; unknown account response remains uniform |
| `email-otp/reset-password` | email, otp, password | consumes recovery code and replaces password; revokes server sessions |

Go rejects OTP on branches without the policy before Compute admission. GET mutation calls, deprecated recovery aliases and private stored-code retrieval routes are excluded. Caller forwarding and Console cookies are stripped. SMTP peer/sender/configuration/credentials are operator input only. No code is echoed by the control API, placed in public reports or persisted in browser storage.

## UI

Before enabling branch Auth, opt into **启用应用邮箱验证码**. The OTP panel selects login/signup, email verification or password recovery, requests delivery, accepts a masked six-digit code and clears code/new password after consumption. Only an actual successful sign-in user populates application-session UI. Verification/reset clears stale displayed identity/JWT; current server state is checked separately. Already issued offline JWTs retain their existing bounded five-minute lifetime.

## Acceptance contract

Protected Linux suite `auth-otp.spec.ts` reuses the accepted owner project/database and authenticated internal mailbox. It must prove:

1. Native UI enable creates the correct opted-in branch runtime.
2. Actual SMTP OTP denies an incorrect code, then creates a verified application identity and session.
3. Sign-in code replay is rejected.
4. A separate existing password user is verified with an actual email code and keeps its identity.
5. Password recovery revokes an independently captured pre-reset cookie, confirmed valid before reset; afterward `get-session` is null and `/token` is 401.
6. Reset replay is rejected; old password is denied and new password works.
7. Normal Auth retirement and owned Compute 0 preserve all accounts, mail and records.

The suite stores actual message bodies and continuation passwords only in protected mode-0600 Linux files. Public evidence contains account IDs, message IDs, operation IDs and check results. Do not recreate a failed account/branch to bypass a failure. Run one product suite at a time.

Disposable PostgreSQL/SMTP tests independently prove hashed values, 300-second expiry, replay, attempt exhaustion across simulated clients, session revocation and the unverified-account ownership guard. Simulated client IPs are used only inside the library fixture; the product Go relay does not trust caller-provided client-IP headers.

## Remaining gates

Actual UI deployment and version-bound evidence, automatic verification-method settings, mail reliability/abuse/privacy, public trusted TLS, distributed rate limiting/failover, OAuth/SSO/MFA and immediate offline JWT revocation remain separate acceptance items. No production certification is asserted.

## Primary sources

- [Neon Email OTP](https://neon.com/docs/auth/guides/plugins/email-otp)
- [Better Auth Email OTP 1.7.7](https://better-auth.com/docs/plugins/email-otp)
- [Existing local SMTP delivery contract](AUTH-EMAIL-RECOVERY.md)
