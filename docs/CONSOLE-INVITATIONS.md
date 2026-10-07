# Console invitations and account registration

## Scope and official alignment

Console accounts administer organizations/projects. They are separate from
database roles, Backend credentials and customer identities in Managed Auth.
Neon's [organization guide](https://neon.com/docs/manage/orgs-api) describes
inviting new/existing accounts. Its [current permission model](https://neon.com/blog/neon-now-has-per-project-permissions)
uses Admin, Editor, Viewer and Collaborator with additive project grants.
Reviewed 2026-10-07; this implementation follows that permission model.

This self-hosted increment delivers manual, account-bound invitation handoff,
invited registration and authenticated acceptance. It does not claim email
delivery, email verification, public signup, OAuth/OIDC, SSO, MFA or Managed Auth.
An invited username that looks like an email is still a local account identifier.
Those integrations require separate Drivers, deployment inputs and acceptance.

## Model and invariants

Migration `012_console_invitations.sql` adds two tables without replacing users,
memberships or existing sessions. Migration is forward-only; retain backups.

| Table | Identity and fields | Invariant |
| --- | --- | --- |
| console_invitations | id, org_id, username, role, invited_by, token_hash, key_hash, request_hash, expiry, accepted_by/at, revoked_at | 256-bit random token, SHA256 hash only; one pending invitation per organization/username; actor/idempotency key unique |
| console_registration_limits | hashed transport peer, minute window, attempt count | Shared PostgreSQL atomic admission, at most 20 attempts/minute; no trusted client-IP header assumption |

Only an active organization Admin with a Console session creates/lists/revokes
invitations. Cookie mutations require CSRF. API keys cannot administer or accept
invitations. Every write locks organization before invitation; membership
mutations use the same lock order. The inviter's current role is checked at
acceptance. Demotion/removal revokes outstanding invitations in the same
transaction, so later promotion cannot revive old delegation.

Registration creates the account, salted password hash, organization membership,
consumed invitation and audit **in one PostgreSQL transaction**. Four competing
acceptances yield one success; another success is forbidden. Revocation and
acceptance serialize on the same organization/invitation locks. Disabled accounts
cannot accept; an existing account must authenticate as itself and its password
is never reset by an invitation.

## API contract

Authoritative schemas and response codes are in `contracts/openapi-v1.json`.
OpenAPI version 0.6.0; bidirectional route parity is part of Linux CI.

| Method and path | Input | Result |
| --- | --- | --- |
| GET /api/v1/organizations/{org}/invitations | Admin session | Up to 200 newest history rows; no token/hash |
| POST /api/v1/organizations/{org}/invitations | CSRF, Idempotency-Key, username, role, expires_hours (1–168) | 201, resource + token once; replay 200, same id, secret_available=false |
| DELETE /api/v1/organizations/{org}/invitations/{invitation} | Admin session + CSRF | 200 idempotent revoke; consumed invitation returns 409, remove member to revoke access |
| POST /auth/signup | token, bound username, password (12–256 characters) | 201 account/membership, sign_in_required=true; no automatic session |
| POST /api/v1/invitations/accept | invited account session, CSRF, token | 201 membership, sign_in_required=false |

Unknown/trailing JSON is rejected. Invalid/expired/revoked/consumed invitations
return `422 invitation_unavailable`; a valid invitation for an existing account
sent to signup returns `409 sign_in_required`. Wrong authenticated account is
403; inaccessible organization is 404. Registration admission cap is 429 with
Retry-After; metadata uncertainty is 503. Same-origin browser POST is required,
and cross-site Fetch/Origin is rejected. Tokens are supplied in request bodies,
never in path/query, operation payloads, metrics or audit bodies.

Every invitation response has Cache-Control:no-store. Idempotent replay cannot
recover plaintext. If the first response is lost, revoke that resource and issue
a new invitation with a new key. The UI retains the key on network failure and
changes it when input changes. Do not retry with a new key hoping to retrieve an
old secret.

## UI and manual verification

1. Sign in as organization Admin. Open **组织与安全 → 成员邀请**.
2. Enter the intended local account, least privileged role and expiry. Create
   invitation. Select/copy the masked one-time credential; transmit it over a
   trusted channel. Close the secret display. Never place it in a shared URL.
3. In a separate browser profile, open the Console login page and **受邀注册**.
   Supply the bound username/invitation and choose/confirm your own password.
4. After registration, log in normally. Check the selected organization and role.
   Viewer cannot manage invitations; Collaborator sees no project without grants.
5. For an existing account, sign in and use **接受组织邀请**. Signup with this
   invitation cannot replace its password.
6. Admin can revoke a pending invitation. Verify it cannot register an account.
   Use **刷新成员** after acceptance in another browser profile.
   Remove an accepted member to revoke access; existing sessions must then get
   404 for this organization even if the user's other organizations remain valid.
7. Check history after reload: accepted/revoked/expired states persist, secrets do
   not reappear. Reissue an expired/revoked invitation with a new key if required.

## Linux verification and resource budget

`api/internal/control/invitations_integration_test.go` runs real PG/middleware
tests: CSRF/API-key/tenant negatives, one-time response, replay/conflict, own
password signup, no existing-account reset, expiry/revoke/demotion, concurrent
consumption, atomic audit and admission cap. Existing membership/last-Admin
tests remain release gates. Disposable PG schemas and job evidence are isolated
from production metadata; no Python verification enters the public repository.

`web/e2e/console-invitations.spec.ts` starts at the actual deployed UI in Linux
Chromium. It covers new account registration/login, another organization's
rejection, viewer UI permissions, existing account acceptance, revoke and member
removal. It creates no Compute. Test orgs/accounts/history are retained as records.
Password fixtures stay in a private mounted directory; reports list only IDs and
checks. Screenshots mask every password/token input; traces/video/HAR are off.

Use the published Linux test runner settings described in TESTING.md. Run one live
suite at a time; collect Job/Pod status/logs before removing finished runtime
resources. Preserve fixtures and all PG/Neon persistent data. Check real-image
provenance and deploy the same API/Worker/Web version before calling an E2E pass.

## Remaining production gates

HTTP laboratory exposure remains unsuitable for untrusted users; production
requires trusted TLS and Secure cookies. Enforce per-client rate limiting at a
trusted ingress in addition to the conservative shared transport-peer cap;
do not trust arbitrary X-Forwarded-For headers. SMTP verification, enterprise
identity, account recovery/session management and external security review remain
separate gates in PRODUCTION-GATES.md. This increment does not certify the full
multi-tenant platform or implement branch Managed Auth.
