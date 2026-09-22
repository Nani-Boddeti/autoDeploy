// Package traefik is the agent-side library for ADR 0005's "Traefik
// configuration ownership" and "Fenced activation and reconciliation":
// deterministic rendering of one environment's dynamic route document,
// atomic same-directory write, and an observation client for Traefik's
// private API. It has no control-plane dependency, performs no database
// access, and is not wired into internal/agent/executor or any HTTP
// handler -- that wiring is Step 10c/10d. Every exported type here excludes
// secret/env material structurally (see EnvironmentRoute's doc comment).
//
// Traefik's file provider accepts YAML or TOML, never JSON (confirmed
// against Traefik's own provider docs during Step 10b; see render.go). This
// package emits hand-rendered, deterministic YAML rather than adding a YAML
// dependency: go.mod carries no YAML library today, and the rendered
// document's shape is small and fixed (one router, one service) rather than
// a general-purpose serialization problem, so a minimal renderer with
// strict per-value quoting is less risk than a new third-party dependency
// for one file shape. See render.go's quoteYAMLString for the quoting rule
// and render_test.go for the injection-proof proof.
//
// # What cannot be proven on macOS
//
// This package's tests prove render determinism/digest stability, atomic
// write/rename/fsync sequencing against a temp directory, and the
// observation client's response parsing against an httptest fake -- all
// pure Go behavior independent of the host OS. They cannot prove:
//
//   - That a real Traefik process, watching a real mounted directory,
//     actually reloads within any particular time after the rename this
//     package performs (Step 10d's real-host gate, per ADR 0005 "Atomic
//     rename is only the request to switch configuration ... reload is
//     asynchronous").
//   - That fsync-then-rename-then-fsync-directory is durable across a real
//     crash on the target filesystem/kernel (ext4 on the production Hetzner
//     host vs. APFS under test here); this package performs the syscalls
//     ADR 0005 specifies and orders them correctly, but crash-durability
//     itself is a property of the underlying filesystem and cannot be
//     exercised by a test process that does not itself crash.
//   - The real shape of Traefik's live API JSON responses for
//     /api/http/routers/{name}@file and /api/http/services/{name}@file;
//     client.go's decode types are built from Traefik's public API
//     documentation and are exercised here only against a synthetic
//     httptest fake, not a running Traefik instance. This is the one
//     genuinely open risk in this step -- see the Step 10b result note for
//     how to close it before flipping activationEnabled.
package traefik
