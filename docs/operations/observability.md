# Observability

What the control plane exposes today: an optional Prometheus-text metrics endpoint, two health
endpoints, and key=value logs in journald. Source: `cmd/controlplane/metrics.go`,
`internal/store/postgres/metrics_snapshot.go`, `internal/logging/logging.go`, `internal/web/web.go`.

## Enabling metrics

Disabled by default. Pass the address to `deploy/host/install-app.sh`, which writes it into
`/etc/autodeploy/controlplane.env` (a generated file: never edit it by hand) and restarts the control
plane if it is running:

```
sudo AUTODEPLOY_METRICS_LISTEN_ADDRESS=127.0.0.1:9101 ./install-app.sh BINDIR
```

Like every other install-app.sh tunable it must be passed on **every** run; a run without it
removes the line and disables metrics. The value must be a **loopback IP literal with a
port** (`127.0.0.1:9101` or `[::1]:9101`); hostnames (including `localhost`), wildcard or
non-loopback addresses, zones and port 0 are refused at startup (`internal/config/database.go`,
`MetricsListenAddressFromEnvironment`). The endpoint has no authentication, which is why it is loopback only.
The control plane also **fails to start if the metrics port cannot be bound** (already in use,
no such address), and `install-app.sh` restarts a running control plane when the configuration changes,
so validate the address first (`ss -ltn` for a free port). Port 8080 is refused (Traefik's API owns it).
Serves `GET` and `HEAD` on `/metrics` only (anything else is 404/405). Scrape it from the host:

```
curl -fsS http://127.0.0.1:9101/metrics
```

## Metrics

| Name | Labels | Meaning |
| --- | --- | --- |
| `autodeploy_build_info` | `version` | Build information; value is always 1. |
| `autodeploy_http_requests_total` | `surface` (`web`, `agent`, `webhook`), `code` (`2xx`..`5xx`) | HTTP requests by surface and status class. |
| `autodeploy_lease_sweep_runs_total` | `result` (`ok`, `error`) | Lease sweeper ticks by result. |
| `autodeploy_leases_expired_total` | none | Agent lease attempts recovered by the sweeper. |
| `autodeploy_log_purge_runs_total` | `result` (`ok`, `error`) | Deployment log purge runs by result. |
| `autodeploy_db_up` | none | 1 if the last metrics snapshot query succeeded, else 0. |
| `autodeploy_agent_jobs` | `state` | Agent jobs by state. |
| `autodeploy_webhook_jobs` | `state` | Webhook deliveries awaiting processing (`state=pending`). |
| `autodeploy_releases` | `state` | Releases by state. |
| `autodeploy_agent_heartbeat_age_seconds` | `server_id` | Seconds since the last heartbeat of each active server that has heartbeated. |
| `autodeploy_environments_reconciliation_required` | none | Environments flagged for operator reconciliation. |
| `autodeploy_release_operations_open` | none | Release operations that are not yet closed. |

All database-derived gauges come from one cached snapshot, so `autodeploy_db_up` and the other gauges
always agree. When the snapshot fails, **only** `autodeploy_db_up 0` is exported; the last good
numbers are deliberately not served, so a dead database never looks healthy.

## What to alert on

Thresholds are suggestions; the repository does not fix them (**operator decision**).

| Condition | Why |
| --- | --- |
| `autodeploy_db_up == 0` for a few minutes | Database unreachable or the snapshot is too slow (see scale note). |
| `autodeploy_agent_heartbeat_age_seconds` > N | Agent down or cut off. The agent's default heartbeat interval is 20 s (`internal/agent/client/run.go`), so N around 90 to 120 s is three to six missed beats. Absent series for a known server also means no heartbeat has ever arrived. |
| `autodeploy_environments_reconciliation_required > 0` | An environment is blocked until an operator resolves it (resolve form on the Reconciliation page, which the sidebar badge counts; see `backup-restore.md` after a restore). |
| `autodeploy_release_operations_open` not decreasing | A release operation (started or switch_authorized) is stuck open; look at the environment and the agent log for that attempt. |
| rate of `autodeploy_http_requests_total{code="5xx"}` > 0 sustained | Server errors on a surface (`web`, `agent`, `webhook`). |
| `autodeploy_lease_sweep_runs_total{result="error"}` or `autodeploy_log_purge_runs_total{result="error"}` increasing | Background maintenance failing. |
| `autodeploy_webhook_jobs{state="pending"}` growing | Webhook deliveries are not being processed. |

**Scale note.** The `agent_jobs` and `releases` counts are full `GROUP BY` scans of those tables on every
snapshot refresh (cached 15 s, failed snapshots reused 5 s; the query runs read-only with a 2 s
server-side `statement_timeout`, 3 s client backstop). That is fine at single-host scale. If
`autodeploy_db_up` flaps to 0 while the database is otherwise healthy (psql works, `/readyz` is `ok`),
the tables have outgrown the snapshot query; the open-operations count already uses an index, the other
two do not.

## Health endpoints

Both are `GET` only (other methods get 405), plain text, served by the control plane's main listener
(`AUTODEPLOY_LISTEN_ADDRESS`, default `172.29.0.1:8081` from `install-app.sh`, plain HTTP behind Traefik).

- `/livez`: always `200 ok` while the process serves requests. No dependency is checked; use it for a
  restart/liveness check.
- `/readyz`: `200 ok` when the control plane can serve. It returns `503 unavailable` when the database
  check fails and, when the secret subsystem is enabled (`AUTODEPLOY_SECRET_SUBSYSTEM_ENABLED=true`), also
  when the KEK manifest and key files cannot be loaded or the active KEK has no ledger row for the
  database generation (`cmd/controlplane/main.go`, `secretLedgerReady`). Use it for "is it usable", for
  example after a restore.

## Logs

Both services log to journald (`journalctl -u autodeploy-controlplane`, `journalctl -u autodeploy-agent`).
Records are slog text, `key=value`, with level and message; the process does not write its own
timestamp, so the journald timestamp is the time of record. Common keys (`internal/logging/logging.go`):
`component`, `attempt_id`, `deployment_id`, `release_id`, `cleanup_id`, `stage`, `class`,
`failure_class`, `error`.

- **Control plane**: fixed messages plus `stage` and `class` only; error text is deliberately not logged,
  because it can carry configuration values. A startup failure logs the failing stage.
- **Agent**: records include the error text, redacted against the agent credential and secret-shaped
  patterns before they are written (a record that cannot be redacted is dropped with the line
  `log record dropped: redaction failed`).

Follow one deployment attempt on the host:

```
journalctl -u autodeploy-agent | grep attempt_id=N
```

(`journalctl -u autodeploy-agent -o cat --since -1h | grep attempt_id=N` for plain lines.) Containers created by an
attempt carry its id in the `io.autodeploy.attempt-id` label (`internal/runtime/docker/labels.go`).

## Agent refuses to claim: corrupt attempt state

At startup the agent reads its durable attempt records (`/var/lib/autodeploy/agent/fences/attempts/`, one
`attempt-<id>.json` per attempt it was executing). If any entry is unreadable, malformed, has unknown fields
or a wrong version, or is not a record file, the agent fails closed. It logs, at error level:

```
agent attempt state unreadable; refusing to claim
```

Heartbeats continue, so the server shows the agent as online but it claims no work (idle). A corrupt fence
file is reported separately (`agent fence state unreadable; refusing to act on environment`).

Recovery:

1. Inspect `/var/lib/autodeploy/agent/fences/attempts/` and identify the offending entry (compare with
   `attempt_id` in `journalctl -u autodeploy-agent`).
2. Move that entry aside, for example into a diagnosis directory outside `attempts/`. Never delete it: it is
   the only evidence of how far the lost attempt got. A leftover `.fence-*.tmp` file is ignored and needs no
   action.
3. Restart the agent: `systemctl restart autodeploy-agent`. A lost attempt whose record you moved aside is
   not acknowledged; its lease expires on the control plane (and blocks the environment if it had reached
   `starting`, see ADR 0004).

## Data retention

The control plane purges history older than 90 days once an hour (single runner across replicas, small batches, runs as the runtime database role): events of finished jobs and route events. Webhook deliveries (and their lifecycle repository rows) are never purged: delivery ids are replay protection. Events of unfinished jobs and the `reconciled` / `activation_late_report` job events (read by activation-resolution repeats) are never purged. Audit tables and `release_operations` are kept forever by design. Failures are logged as `event retention purge failed`.
