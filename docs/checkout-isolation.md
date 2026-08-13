# Privileged checkout isolation

## Boundary and architecture

The application agent never receives a checkout path, a shell command, a Git
binary path, a project ID, or authority to alter cgroups. `gitcheckout` is the
production boundary: `NewProductionRunner` connects only to two fixed,
root-owned Unix `SOCK_SEQPACKET` services.

- `checkout-quota-service` allocates an XFS project quota and passes an open
  workspace directory descriptor. It later revokes and removes that workspace
  only when given the same descriptor and opaque handle.
- `checkout-exec` accepts one of the fixed Git operations, creates a cgroup-v2
  leaf, and runs the already trusted Git executable as the checkout identity.
- The agent process can hold descriptors and connect to sockets. It must never
  own the workspace root, quota ledger, helper binaries, helper unit files, or
  the cgroup parent.

There is no non-Linux, local-process, path-based, or cgroup-v1 fallback. A
missing helper, untrusted socket, invalid configuration, or unavailable kernel
facility causes checkout to fail closed.

## Identities and filesystem layout

Create separate system identities: a long-lived application `agent` user and a
non-login `checkout` user. They must have distinct, non-root UIDs and distinct
primary GIDs. Give only the agent the dedicated `agent` group that is permitted
to connect to the two helper sockets. The checkout user's primary group is its
separate checkout GID and it must not be in the socket-connect group; do not
grant either identity sudo, root group membership, or write access to helper
state.

Use root-owned, non-group/world-writable directories for the checkout root,
runtime socket parent, binary directory, and quota state directory. Socket
files are root-owned and group-connectable only by the configured agent group.
The checkout user must not be able to enumerate or alter the quota state,
socket parent, cgroup parent, parent cgroup controls, or root-owned checkout
root. Keep helper executables and their parent directories root-owned and not
writable by group/world.

Verify this before enabling the agent (substitute local paths and names):

```text
id agent; id checkout
stat -c '%U:%G %a %n' /run/... /run/.../*.sock /var/lib/... /srv/...
namei -l /run/.../checkout-quota.sock
findmnt -T /srv/... -o TARGET,FSTYPE,OPTIONS
```

The two UIDs and primary GIDs must differ, and only the agent group may have
socket connect permission. Never place credentials in these paths or in helper
command lines.

## XFS project quotas and ledger initialization

The checkout root must be a dedicated XFS filesystem mounted with `prjquota`
(also called `pquota` on some systems). Do not use a bind mount whose backing
filesystem lacks project quotas. Confirm the mount options with `findmnt`; the
quota service also rejects a non-XFS filesystem or quotas that the kernel does
not report as enabled.

Choose a documented, unused high-water project ID. It is a one-time migration
input, not a value that should be guessed or reused. With helpers stopped and
an empty, root-owned state directory, initialize the ledger once:

```text
checkout-quota-service --initialize --initial-project-id <chosen-high-water-id> \
  <checkout-root> <quota-socket> <state-file> <agent-uid> <agent-gid> \
  <checkout-uid> <checkout-gid> <maximum-bytes> <maximum-inodes>
```

Then remove `--initialize`; a normal service startup loads and reconciles the
ledger. The ledger binds the configured checkout UID/GID, so changing either
identity requires a deliberate migration rather than a helper restart. Back up
the root-owned quota ledger atomically while the service is
stopped, or use a filesystem-consistent backup. Preserve its permissions and
the mounted filesystem identity. Never edit the ledger manually or restore it
onto a different filesystem without the documented recovery procedure.

## cgroup v2 delegation and spawn service

Mount unified cgroup v2 and delegate one dedicated, non-shared cgroup parent
to the root `checkout-exec` service. Enable `cpu`, `io`, `memory`, and `pids`
in that parent's `cgroup.subtree_control` before starting the service. The
parent must be writable only by root or the specifically delegated service
identity; neither checkout nor the application agent may write it. Check with:

```text
stat -fc %T <cgroup-root>
cat <cgroup-root>/cgroup.controllers
cat <cgroup-root>/cgroup.subtree_control
```

Start `checkout-exec` with absolute socket, trusted binary directory, cgroup
root, agent UID/GID, distinct checkout UID/GID, and nonzero bounds for memory,
PIDs, I/O, CPU quota/period, duration, output, and block device major/minor.
The service rejects incomplete bounds. The Git executable is opened from the
trusted root-owned directory at startup; do not point it at an arbitrary PATH.

## Service ordering and hardening

Run both helpers as root-owned systemd services, not from the agent process.
The quota service must start after the XFS mount and state directory are ready.
The spawn service must start after cgroup v2 delegation and its trusted binary
directory are ready. The application agent starts only after both socket files
exist with expected ownership and mode.

Apply service hardening compatible with the needed kernel interfaces: a
minimal writable-path allowlist, `NoNewPrivileges`, private temporary storage,
restricted address families (Unix only for the helpers), and capability/syscall
allowlists tested on the target distribution. Do not weaken the helpers merely
to make a unit start. Ensure unit ordering restarts the agent after either
helper changes, and remove stale sockets only from the root-owned runtime
directory during helper startup.

Readiness is the successful creation of trusted socket metadata and a helper
that can authenticate as root over its local peer credential—not merely an
open port. Configuration or readiness failure must keep checkout disabled.

## Crash handling, cleanup, and recovery

An allocation acknowledgement can be lost. The agent retains the returned
descriptor/handle and resolves or retries cleanup; it must not invent a path.
On quota-service restart, reconciliation handles incomplete records. Treat
damaged or ambiguous ledger records as quarantined: investigate the workspace
and project quota before any manual removal. Project IDs are monotonic and are
not recycled.

If cleanup repeatedly fails, keep the checkout unavailable to its caller and
retry with the same capability. For planned recovery: stop the agent, stop the
helpers, take a permissions-preserving ledger backup, inspect XFS quota state
and stale cgroup leaves, reconcile through a normal quota-service startup, and
only then restart helpers followed by the agent. Do not delete workspaces,
ledger rows, cgroups, or sockets as the checkout user.

## Privileged integration verification

Ordinary CI intentionally does not exercise host quotas, root peer checks, or
cgroup delegation. It only cross-compiles the Linux helpers and runner. On a
provisioned Linux host, run the test as the non-root agent UID, with its
primary group equal to the configured agent GID, after both root helpers are
ready. The test separately requires distinct configured checkout UID and GID
values; the quota helper grants the workspace to that identity and the spawn
helper enforces it for its Git child. It proves active workspace ownership,
performs harmless `git init`, releases it, and checks that no
checkout cgroup leaf remains; it does not contact GitHub and needs no token.

Set explicit non-secret configuration values for all of these, then run the
command. Missing opt-in/configuration fails; absence of the opt-in causes the
test itself to report a skip when called directly.

```text
AUTODEPLOY_CHECKOUT_INTEGRATION=1 \
AUTODEPLOY_CHECKOUT_QUOTA_SOCKET=/absolute/quota.sock \
AUTODEPLOY_CHECKOUT_SPAWN_SOCKET=/absolute/spawn.sock \
AUTODEPLOY_CHECKOUT_CGROUP_ROOT=/absolute/cgroup-root \
AUTODEPLOY_CHECKOUT_AGENT_UID=<agent-uid> \
AUTODEPLOY_CHECKOUT_AGENT_GID=<gid> \
AUTODEPLOY_CHECKOUT_UID=<checkout-uid> \
AUTODEPLOY_CHECKOUT_GID=<checkout-gid> \
AUTODEPLOY_CHECKOUT_QUOTA_BYTES=<bounded-bytes> \
AUTODEPLOY_CHECKOUT_QUOTA_INODES=<bounded-inodes> \
AUTODEPLOY_CHECKOUT_TIMEOUT_SECONDS=<seconds> \
AUTODEPLOY_CHECKOUT_MAX_OUTPUT=<bytes> \
make test-checkout-integration
```

This is a runtime smoke test, not a substitute for inspecting mount options,
unit hardening, recovery drills, backups, and production monitoring.
