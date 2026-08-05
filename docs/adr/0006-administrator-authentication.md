# ADR 0006: Authenticate the Initial Administrator with Server-Side Sessions

- Status: Accepted
- Date: 2026-08-05

## Context

AutoDeploy needs an authenticated administrator before it can safely expose project, repository,
environment, secret, or deployment operations. V1 has one administrator, but the authentication
model must retain stable owner and user identities so later multi-user work does not require an
authorization rewrite.

The public control plane is exposed through Traefik over HTTPS. It cannot trust client-supplied
forwarding headers by default, cannot put durable credentials in environment variables, and must
remain safe across process restarts and multiple control-plane replicas. ADR 0002 also requires a
GitHub App setup callback even though the administrative session cookie is deliberately
`SameSite=Strict`.

This ADR defines bootstrap, password verification, server-side sessions, CSRF defense,
authorization, throttling, recovery, proxy trust, and audit boundaries. It intentionally exposes
only login, logout, a minimal authenticated landing page, and health endpoints in this slice.
Deployment administration remains unavailable until deployment records have explicit owner
scope.

## Decision

AutoDeploy uses one permanently bootstrapped owner account, Argon2id password verifiers, opaque
server-side sessions stored by digest in PostgreSQL, synchronizer-token CSRF protection, durable
login throttling, and exact owner-scoped authorization.

The administrator is created and recovered only by a local operator CLI with access to the same
trusted credential and database boundary as the control plane. There is no public bootstrap,
password-reset email, recovery link, default password, or environment-variable password path.

## Identity and bootstrap

V1 creates one immutable owner and one administrator user. IDs are application-generated opaque
identifiers. After trimming ASCII space and lowercasing ASCII letters, usernames must match
`[a-z0-9][a-z0-9._-]{2,63}` exactly: 3 to 64 ASCII bytes, beginning with an alphanumeric byte.
Other whitespace, non-ASCII input, and Unicode lookalikes are rejected. Canonical username
uniqueness is enforced in PostgreSQL. Owner ID, user ID, and role are separate fields; the owner
role is not an implicit global-scope bypass.

The operator commands are:

- `admin bootstrap --username <name>`
- `admin reset-password --username <name>`

Passwords are read twice from a verified interactive terminal without echo. Password values are
never accepted through arguments, standard input pipes, environment variables, config files,
logs, or the web application. Commands refuse non-terminal input and return sanitized errors.

Bootstrap is permanently one-time. A singleton installation-authentication row and PostgreSQL
transaction/advisory lock ensure concurrent bootstrap attempts create exactly one owner and user.
Deletion or deactivation does not reopen bootstrap. Restoring a database preserves the completed
marker.

Password reset locks the user, increments its authentication revision, replaces the verifier,
revokes every active session, clears login-throttle state needed for recovery, and appends a
sanitized audit event in one transaction. A partial reset is rolled back. Recovery requires
trusted host/database access and is not available over HTTP.

## Password policy and verification

Passwords contain 15 to 1,024 UTF-8 bytes. Unicode and spaces are allowed. V1 applies no
composition rules and no Unicode normalization, because silent normalization can change a user's
secret. Empty, invalid UTF-8, too-short, and oversized values are rejected before hashing.

Password verifiers use an Argon2id PHC string with a cryptographically random salt and parameters
embedded per hash. New hashes meet at least 19 MiB memory, two iterations, and one lane. The
bootstrap command calibrates once, before accepting concurrent bootstrap, toward approximately
250 milliseconds within explicit implementation bounds of 19–64 MiB memory, two to five
iterations, and one lane. The winning bootstrap transaction persists one installation-wide
password-policy revision and its parameters. All replicas and password resets read that durable
policy; startup never recalibrates it. Changing the policy requires a future explicit operator
workflow and monotonically higher revision, preventing heterogeneous replicas from repeatedly
rehashing one another's output.

The parser accepts only the supported Argon2id version and strictly bounded parameters. It rejects
malformed, duplicate, missing, non-canonical, or trailing fields before allocating memory. Verify
ceilings are 256 MiB memory, 10 iterations, and four lanes. Values below the current policy can
authenticate but are marked for controlled rehash; values outside parser ceilings fail closed.
Implementation must use the latest stable, non-deprecated Go cryptography dependency verified
against its official source at implementation time.

Unknown users execute a fixed valid dummy Argon2id verification. Login returns the same status,
body, and redirect behavior for unknown username, wrong password, disabled user, stale revision,
and throttled credentials. Timing is best-effort equalized and must not be described as perfectly
constant time.

## Session model

Successful login rotates any pre-authentication state and creates a new opaque token with the
format `v1.` followed by 32 cryptographically random bytes encoded as unpadded base64url. Only a
domain-separated SHA-256 digest is stored in PostgreSQL. Raw tokens are never logged, audited,
persisted in jobs, or returned outside the cookie.

The cookie is named `__Host-ad_session` and always has `Secure`, `HttpOnly`, `SameSite=Strict`, and
`Path=/`; it has no `Domain` attribute. Production startup requires an explicit canonical HTTPS
origin. Login and logout use fixed same-origin `303 See Other` destinations and never accept an
arbitrary return URL.

Each session records user ID, owner ID, authentication revision, creation time, last-seen time,
idle expiry, absolute expiry, and revocation state. PostgreSQL `clock_timestamp()` is authoritative.
Defaults are:

- 30-minute idle timeout.
- Eight-hour absolute lifetime.
- At most ten active sessions per user; a successful new login revokes the oldest excess sessions
  in the same transaction.
- Last-seen persistence at most once per five minutes to reduce write amplification, while every
  request still validates both expiries against database time.

There is no periodic session-token renewal. Password reset or any security-sensitive credential
change increments the user's authentication revision, immediately invalidating sessions carrying
an older revision. Logout is POST-only, requires the ordinary CSRF checks, revokes the server-side
session, and clears the cookie. Cookie deletion is not the authorization boundary.

## CSRF and browser request validation

Authenticated sessions use a synchronizer token derived with HMAC from server-held signing key
material and stable session context. The key is supplied through the external mounted credential
provider defined by ADR 0003, supports rotation, and is never stored in PostgreSQL or an
environment-variable value. Tokens are domain-separated and compared in constant time.

The login form uses an independent `__Host-ad_login` cookie containing 32 random bytes encoded as
unpadded base64url. It is `Secure`, `HttpOnly`, `SameSite=Strict`, `Path=/`, has no `Domain`, and
expires after ten minutes. The hidden token is a versioned, domain-separated HMAC over the raw
cookie nonce and canonical origin using the external CSRF signing-key ring. A still-valid nonce is
reused so multiple browser tabs do not invalidate each other. `GET /login` replaces absent,
expired, or malformed state; `POST /login` never mints replacement state. Failed credentials keep
valid state, while successful login clears it and creates unrelated session state. The cookie and
form token can never become an authenticated session or authorize another origin.

Every unsafe browser-session request requires all of:

- An exact match between a present `Origin` and the configured canonical HTTPS origin; only when
  `Origin` is absent may an exact same-origin HTTPS `Referer` be used. Malformed, opaque, `null`,
  downgraded, cross-origin, or wholly absent provenance fails closed.
- The synchronizer token from the expected form/header location.
- An allowlisted content type and a bounded request body.
- A valid, unexpired, non-revoked session and current authentication revision.

These browser CSRF checks are attached only to browser-session routes. GitHub webhooks and future
agent APIs use their own authentication and replay defenses and must not be accidentally rejected
or authorized by browser middleware.

## GitHub App Strict-cookie handoff

The Strict session cookie is intentionally not weakened for GitHub's cross-site installation
callback. The setup flow uses two one-time server-side capabilities:

1. A session- and CSRF-protected POST stores a short-lived digest of random state bound to the
   session, user, owner, and authentication revision, then redirects to GitHub.
2. The callback, which may arrive without the Strict session cookie, validates and consumes the
   state and verifies installation identity through an authenticated GitHub API lookup or verified
   lifecycle webhook evidence. It may only transition the record to a one-time handoff.
3. The callback sets a separate short-lived Strict handoff cookie and redirects to a clean
   same-origin landing URL without capability data.
4. The user explicitly confirms from that page. Confirmation requires the administrative session,
   handoff cookie, CSRF token, current authentication revision, and a compare-and-set consume.

The callback never binds a repository or installation and never mutates authorization directly.
State and handoff capabilities expire within ten minutes, are stored only by domain-separated
digest, are single-use under concurrency, and cannot cross users or owners. Callback and landing
responses use `Cache-Control: no-store`, a no-referrer policy, no third-party resources, and logs
that exclude query strings.

## Durable login throttling

Throttling state lives in PostgreSQL so restart or another replica cannot reset it. Before Argon2
work, the login transaction reserves a bounded attempt against normalized, privacy-minimized
buckets; after verification it records success or failure. A crash after reservation counts
conservatively as a failed attempt.

Defaults are:

- Username/client pair: five failures per 15 minutes.
- Client IP: 20 attempts per 15 minutes.
- Username-wide exponential delay after ten failures, beginning at 30 seconds and capped at 15
  minutes.

Updates are atomic under concurrency. Successful login clears only the appropriate credential
failure state, not unrelated abuse evidence. Stored client identifiers use keyed, rotated,
domain-separated digests where raw addresses are unnecessary. Each digest carries a non-secret key
version. Reservation checks every non-retired key version, aggregates matching live buckets, and
writes the active version; rotation keeps the prior key readable for at least the longest throttle
window plus clock/cleanup margin and cannot retire it while live buckets remain. Rotation does not
reset counters, split limits, or permit double capacity. Raw addresses may be retained only after
explicit approval of a separate audit/retention policy. Responses remain generic and do not reveal
which bucket applied.

Throttle storage uses expiring fixed-window rows, bounded key lengths, bounded time ranges, and a
configured maximum active-row cardinality. Once the cap is reached, unseen identities are folded
into bounded per-window overflow buckets rather than creating rows without limit. Overflow remains
conservative and never grants more attempts. Cleanup removes expired rows in bounded batches, but
admission remains bounded when cleanup is delayed.

## Proxy and client-address trust

The direct peer address is authoritative unless it falls within an explicitly configured trusted
proxy CIDR. The default trusted-proxy list is empty. Only then may the control plane parse a bounded
`X-Forwarded-For` chain from right to left, discarding known proxies and selecting the first
untrusted address. An invalid, oversized, empty, or ambiguous chain from a trusted proxy is charged
to a bounded invalid-forward bucket derived from that proxy and rejected before credential work;
it never silently shares the proxy's ordinary client bucket. When the direct peer is untrusted,
forwarding headers are ignored rather than parsed.

Traefik must configure explicit `trustedIPs`; insecure forwarded-header trust is forbidden.
IPv4-in-IPv6 representations are canonicalized before CIDR checks. Forwarded host and scheme are
not accepted as substitutes for the configured canonical public origin.

## Authorization and transaction boundaries

Authenticated request context contains only non-secret principal data: user ID, owner ID, role,
authentication revision, and a non-secret internal session ID. Resource access requires exact
owner-ID equality plus the operation-specific permission. An owner role does not bypass missing or
mismatched ownership.

Authentication middleware may reject early, but every state-changing transaction must re-lock and
recheck the session, user status, authentication revision, and resource owner before committing.
Security-sensitive transactions use a documented lock order so password reset, session creation,
authorization changes, and future resource mutations cannot create a stale-authority commit.

This slice does not add deployment reads or mutations because the current deployment aggregate
does not yet carry owner ID. The only application routes are:

- `GET /login`
- `POST /login`
- `POST /logout`
- protected `GET /` with a minimal authenticated landing page
- narrowly scoped liveness and readiness endpoints without sensitive diagnostics

## Persistence and audit

A forward-only additive migration introduces:

- `owners`
- a singleton `installation_auth` bootstrap marker
- `users` with canonical username, role, password PHC string, status, and authentication revision
- `admin_sessions` with token digest, scope, revision, expiries, and revocation metadata
- `auth_throttle_buckets`
- append-only `auth_audit_events`
- short-lived GitHub setup state and handoff records when that flow is implemented

The migration requires no PostgreSQL extension. Constraints enforce known roles/statuses, positive
revisions, digest sizes, timestamp ordering, singleton bootstrap, and bounded field lengths.
Indexes support digest lookup, active-session expiry/limits, throttle updates, and capability
cleanup.

Audit events record bootstrap, login success, logout, session revocation, password reset,
authenticated recovery actions, and future handoff transitions. Unauthenticated login failures
are coalesced into the bounded expiring throttle rows and bounded operational counters; arbitrary
public input never creates one permanent audit event per request. A transition into or out of a
throttled state may emit at most one sanitized event per existing admitted bucket/window. Audit
events contain stable actor/owner identifiers and sanitized metadata, never submitted usernames,
passwords, verifiers, raw/digested tokens, CSRF values, request bodies, cookies, authorization
headers, client addresses, or query strings. Database triggers reject audit update and delete
until a separately approved retention/archive policy exists.

Expired sessions, throttle windows, and setup capabilities are deleted by bounded, restartable
cleanup. Authentication correctness never depends on cleanup running on time.

## HTTP and operational safety

Responses set a restrictive content security policy, frame denial, MIME sniffing protection,
referrer policy, and `Cache-Control: no-store` on authentication pages. Templates use contextual
escaping. Errors shown to clients are generic; structured logs use request IDs and sanitized
security outcomes.

The server configures bounded header/body reads, read/write/idle timeouts, maximum header size,
graceful shutdown, and readiness that fails when required database or credential providers are
unavailable. Health endpoints disclose no version, configuration, dependency error, or secret
detail.

Production database and signing credentials are read from validated, permission-hardened mounted
files. Environment variables may name non-secret file paths and public configuration, but cannot
contain credential values. Missing, symlinked, group/world-readable, oversized, malformed, or
wrong-owner credential files fail closed according to ADR 0003.

## Failure handling

- Database unavailable: login and authenticated requests fail closed; readiness fails.
- Signing key unavailable or unknown version: CSRF validation fails closed; readiness fails.
- Malformed session, CSRF, PHC, forwarding, or capability input: reject without panic or unsafe
  allocation.
- Reset racing login: row locks and authentication revision prevent creation or use of a stale
  session.
- Concurrent bootstrap: exactly one transaction succeeds; later attempts report already
  bootstrapped without disclosing account details.
- Audit write failure: the associated security mutation rolls back.
- Clock uncertainty: PostgreSQL time governs durable expiry; application time cannot extend it.

## Consequences

- Compromise of the database alone does not reveal passwords or usable session tokens, though an
  offline password attack remains possible and motivates strong Argon2id parameters.
- Server-side sessions provide immediate revocation and durable replica-safe enforcement at the
  cost of a database check for authenticated requests.
- Strict cookies and exact-origin checks reduce browser attack surface but require the explicit
  GitHub handoff flow.
- Host administrators remain trusted because they can access mounted credentials and the running
  process, consistent with ADRs 0001 and 0003.
- A later multi-user release can add roles and memberships without changing owner-scoped resource
  identity.

## Rejected alternatives

- Public first-user bootstrap or web password reset: exposes a takeover/recovery surface.
- Passwords in flags, stdin pipes, environment values, or repository config: leak through process,
  shell, CI, or deployment metadata.
- Stateless signed session cookies: make immediate revocation, idle enforcement, and session caps
  harder and expose more authority client-side.
- Storing raw session/capability tokens: a database disclosure would provide live bearer
  credentials.
- `SameSite=Lax` solely for GitHub callbacks: weakens the default browser boundary when a bounded
  handoff preserves Strict cookies.
- In-memory throttling: resets on restart and is inconsistent across replicas.
- Trusting all forwarded headers: permits client-address and origin spoofing.
- Adding deployment handlers before owner scope exists: would create an authorization ambiguity.
- External breached-password lookup in V1: introduces privacy and availability dependencies; it
  may be added later behind an explicitly approved policy.

## Verification requirements

Implementation is not complete until automated tests cover:

- Argon2id round trips, malformed/unknown PHC data, parser ceilings, rehash signaling, Unicode,
  byte-length bounds, and dummy verification.
- Session token format/entropy boundary, digest-only storage, fixation prevention, session cap,
  logout/revocation, idle timeout, absolute expiry, and stale authentication revision.
- Login and authenticated CSRF failures for missing, wrong, stale, cross-session, cross-origin,
  null, absent, malformed, and wrong-content-type requests; required cookie attributes.
- Generic login behavior and bounded timing comparisons for unknown, wrong, disabled, throttled,
  and successful cases.
- Concurrent singleton bootstrap, reset-versus-login serialization, throttle concurrency,
  restart/multi-replica durability, and audit rollback.
- Trusted and spoofed forwarding chains across IPv4, IPv6, mapped addresses, malformed input, and
  the empty-trust default.
- Throttle digest-key rotation with active buckets, overflow/cardinality limits under adversarial
  unique identities, delayed cleanup, and bounded audit growth.
- GitHub handoff replay, expiry, wrong user/owner/session/revision, failed installation
  verification, concurrent consume, and Strict-cookie behavior after the cross-site return.
- Exact owner authorization, cross-owner denial, stale revision denial, methods, request limits,
  fixed redirects, headers, timeouts, graceful shutdown, and non-sensitive health responses.
- Migration concurrency/checksum behavior, schema constraints, append-only audit enforcement, and
  bounded cleanup.

Run unit tests, race tests, PostgreSQL integration tests, static checks, formatting, and diff
checks. Complete an independent security-focused code review before exposing any additional
administrative route.
