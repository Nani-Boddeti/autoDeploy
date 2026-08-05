# ADR 0002: Authenticate GitHub Access with a Private GitHub App

- Status: Accepted
- Date: 2026-08-05

## Context

AutoDeploy must receive push events from private GitHub repositories and let a deployment agent
fetch the exact authorized commit. Long-lived personal access tokens, user passwords, broad SSH
keys, and App-wide credentials would create unnecessary scope and couple automation to a human
account.

The V1 repositories are all owned by the same GitHub account that owns the GitHub App. AutoDeploy
has one administrator initially, but repository authorization must remain explicit and must not
depend on mutable repository names, webhook senders, or possession of a signed payload alone.

ADR 0001 requires the control plane to own authorization and the agent to perform the authorized
checkout without receiving control-plane-wide credentials.

## Decision

AutoDeploy uses a private GitHub App with installation-to-server authentication. The App is
private because V1 does not install into repositories owned by any account or organization other
than the App owner. Supporting other owners requires a new reviewed decision and a publicly
installable App configuration.

The control plane owns App authentication, installation/repository authorization, webhook
verification, and short-lived installation-token minting. An agent receives only a transient,
single-repository, read-only installation token for one authorized checkout.

## App registration, permissions, and events

The GitHub App requests the minimum explicit repository permission:

- Contents: read.

The App explicitly subscribes only to:

- Push.

The control plane also handles the installation and installation-repositories lifecycle events
that GitHub sends to every App. Adding permissions or event subscriptions requires a reviewed ADR
amendment and corresponding authorization tests.

Repository owners should select only the repositories intended for AutoDeploy. Local project
binding remains mandatory even if GitHub repository selection is later broadened.

## Installation and repository identity

- Persist GitHub installation, account, and repository numeric IDs as stable external identities.
- Treat owner names, repository names, and full names as mutable display metadata.
- Bind one active local project to an explicitly selected installation and repository ID.
- Record repository-selection, suspension, permission-snapshot, and reconciliation metadata.
- Do not authorize a repository merely because the App installation can access it.
- Do not authorize from the webhook sender identity.

An authenticated AutoDeploy administrator initiates installation using one-time, expiring state.
The setup callback and its installation ID are untrusted input. The control plane establishes or
updates an installation only after a verified lifecycle webhook or an App-authenticated GitHub
API lookup, followed by explicit administrator repository binding.

## Push authorization

A verified push can create deployment intent only when all of these remain true in one authorized
transaction:

- The GitHub App registration is recognized and active.
- The installation ID is recognized, active, and not suspended.
- The repository ID is currently accessible to that installation.
- The repository is explicitly bound to an active local project and owner scope.
- The full ref exactly equals the project's configured `refs/heads/<branch>` value.
- The push is not a branch deletion and has a nonzero `after` commit SHA.
- The delivery ID has not already produced the same authorized state change.

Repository and installation authorization is rechecked before minting a checkout token. A valid
webhook is evidence of origin, not continuing authorization.

## App and installation authentication

The control plane signs RS256 App JSON Web Tokens in memory with the App private key. JWT issue
time is backdated for clock skew and expiry remains within GitHub's maximum ten-minute window.
JWTs are never persisted.

For checkout, the control plane requests an installation access token narrowed to:

- exactly one authorized repository ID;
- Contents: read.

The response scope, permission set, and expiry are validated before use. Installation tokens are
treated as opaque values, expire within GitHub's one-hour lifetime, and are minted near checkout.
They must not appear in URLs, process arguments, Git configuration, durable jobs, PostgreSQL,
audit records, API responses, or logs.

The token is delivered transiently over the authenticated agent channel, used through a
confidentiality-protected transport with server identity verification. Plaintext transport and
disabled certificate verification are forbidden. Proxies and intermediaries must not log
authorization material. The agent uses a credential/askpass mechanism that avoids command-line
exposure, clears the token after checkout, and requests best-effort revocation. The App private
key and App JWT are never sent to an agent.

On checkout authentication failure:

- A 401 discards the token and permits one fresh mint only if the lease and authorization still
  remain valid.
- A 403 is classified before action: `Retry-After`, `X-RateLimit-Remaining`,
  `X-RateLimit-Reset`, and the response body distinguish rate limiting from authorization
  failure. A non-rate-limit 403 or a 404 fails closed and triggers reconciliation.
- A 429 or classified rate-limit 403 honors GitHub retry/reset guidance with bounded backoff.
- Suspension, removal, uninstall, lease loss, or cancellation prevents minting and retry.

## Webhook verification and durable receipt

- Accept webhook traffic only over HTTPS in production.
- Read a bounded raw request body exactly once.
- Require and validate the event and delivery headers.
- Verify `X-Hub-Signature-256` with HMAC-SHA256 and constant-time comparison before JSON parsing.
- Reject malformed payloads, unsupported events/actions, or missing stable identifiers.
- Use `(GitHub App registration ID, X-GitHub-Delivery)` as the durable delivery-attempt key.
- Independently index the verified raw-body digest per App registration. The same signed body
  under a different delivery ID is rejected and audited as a replay/integrity violation.
- Enforce domain uniqueness for automatic push intent using the active project binding,
  repository ID, full ref, and `after` commit SHA. A webhook cannot automatically deploy the
  same commit/ref twice; an administrator-requested redeploy is a separate authenticated intent.
- Store the payload digest and safe normalized identifiers, event/action, verification status,
  processing status, and sanitized failure details.
- Treat the same delivery ID with a different payload digest as an integrity violation.
- Commit the durable receipt and authorized normalized state change before returning success.
- Return promptly after durable receipt and process asynchronous work through durable jobs.

Do not persist an unrestricted raw webhook payload. Normalized storage must exclude credentials
and minimize personal or repository content not required for authorization, audit, or replay-safe
processing.

GitHub does not automatically redeliver every failed delivery. V1 uses GitHub's manual redelivery
plus local installation/repository reconciliation; automated failed-delivery recovery is deferred.

## Credential storage and rotation

Control-plane-only secrets:

- GitHub App private key.
- Webhook secret.

The production baseline loads them from service-readable mounted files outside the repository,
behind credential-provider interfaces that fail closed when credentials are missing or
unreadable. File ownership, permissions, replacement, symlink, and backup rules are binding
decisions for ADR 0003. Plaintext secret values are never stored in PostgreSQL or returned by the
UI/API. A future sign-only key-vault adapter may replace mounted private-key files without
changing the authentication boundary.

Private-key rotation is add new key, deploy and verify its use, then revoke the old key. Rotation
must take advantage of GitHub's overlapping active private keys rather than introduce downtime.

Webhook-secret rotation temporarily accepts the old and new local secrets during a bounded
overlap, updates GitHub to the new secret, verifies new deliveries, and then removes the old
secret. Rotation events record identifiers and status, never secret material.

## Revocation, expiry, and reconciliation

Installation suspension or deletion:

- Mark the installation unavailable transactionally.
- Reject new pushes and token minting.
- Cancel or fail queued/fetching work according to the future job policy.
- Preserve the currently healthy active release and audit history.

Repository removal:

- Disable affected project bindings immediately.
- Reject later push processing and token requests.
- Preserve historical deployment records.

Repository addition or installation unsuspension does not silently authorize deployment. It
requires successful reconciliation and explicit local binding or administrator reactivation.

Periodic reconciliation detects missed lifecycle deliveries, repository rename/transfer, changed
permissions, and installation state drift. Reconciliation never expands local authorization
without explicit administrator action.

## Private submodules and large-file storage

V1 rejects private cross-repository submodules unless every referenced repository is installed,
explicitly bound, and independently authorized for credential use. Token forwarding across
repositories is forbidden. Git LFS and submodule behavior require explicit checkout tests in the
future Git adapter slice.

## Consequences

Benefits:

- Automation is independent of a human GitHub account token.
- Repository access is short-lived, read-only, and narrowed per checkout.
- Installation lifecycle events provide a revocation and reconciliation boundary.
- The agent never holds credentials capable of minting tokens for other installations.

Costs:

- Installation state, repository binding, reconciliation, token minting, and key rotation become
  explicit control-plane responsibilities.
- Webhook verification alone is insufficient; every push requires local authorization checks.
- Supporting repositories outside the App owner's account requires changing App visibility and
  reviewing tenant ownership verification.

## Alternatives considered

- Personal access tokens: rejected because they are user-bound and encourage broader, longer-lived
  access.
- Per-repository deploy or SSH keys: rejected because installation ownership, webhook lifecycle,
  and scalable rotation remain separate problems.
- User OAuth tokens for runtime automation: rejected for V1; future multi-tenant installation
  ownership verification may require a distinct user authorization flow.
- App-wide or all-repository installation tokens: rejected because GitHub permits narrowing by
  repository and permission.
- App private key on agents: rejected because it can mint access across App installations.
- Proxy source archives through the control plane: deferred because it increases control-plane
  bandwidth/storage and conflicts with agent-owned checkout in ADR 0001.

## Deferred decisions

- GitHub SDK/client implementation and HTTP retry adapter.
- Database schemas and repositories for installations, projects, and webhook deliveries.
- Installation setup routes, administrator UI, and reconciliation schedule.
- Exact payload/body limits and normalized retention periods.
- Agent credential-delivery protocol and in-memory checkout adapter.
- Automated GitHub failed-delivery recovery.
- Public or multi-owner GitHub App operation.

## Validation criteria

- App registration exposes only Contents: read and the explicit Push subscription.
- Invalid or missing signatures fail before payload parsing.
- Duplicate/redelivered delivery IDs create at most one authorized deployment intent.
- A reused delivery ID with a different digest is rejected and audited.
- The same signed body replayed under a different delivery ID is rejected and audited.
- A different valid delivery for the same project/repository/ref/commit cannot create a second
  automatic deployment; manual redeploy uses a separate authenticated intent.
- Wrong installation, repository, owner scope, ref, or deletion SHA cannot enqueue work.
- A spoofed setup callback cannot create or bind an installation.
- Repository removal or installation suspension immediately blocks token minting.
- A minted token is one-repository/read-only, expires within one hour, and is never persisted or
  logged.
- Agent token delivery is confidentiality-protected, verifies server identity, and is not logged
  by intermediaries.
- Rate-limit 403 responses are distinguished from authorization failures before reconciliation.
- Key and webhook-secret rotation complete without accepting unverified deliveries.
- Checkout authentication retries at most once and only with current lease and authorization.
- Failed persistence returns a non-success webhook response; successful durable receipt returns
  within GitHub's delivery timeout.

## References

- [About authentication with a GitHub App](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/about-authentication-with-a-github-app)
- [Choosing permissions for a GitHub App](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/choosing-permissions-for-a-github-app)
- [Generating a JSON Web Token](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-json-web-token-jwt-for-a-github-app)
- [Generating an installation access token](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app)
- [Validating webhook deliveries](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries)
- [Webhook best practices](https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks)
- [Webhook events and payloads](https://docs.github.com/en/webhooks/webhook-events-and-payloads)
- [Managing private keys](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/managing-private-keys-for-github-apps)
