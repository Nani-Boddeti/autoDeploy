// Package docker is an Engine API client and label authority for AutoDeploy's
// host agent. It talks to the Docker daemon over its Unix domain socket
// using only the standard library, and it is the single place that builds
// and matches the Docker labels ADR 0005 requires for cleanup-by-exact-label.
package docker

import (
	"errors"
	"fmt"
)

// ErrUnsupportedPlatform is returned by NewClient on any platform other than
// linux. The agent's Docker Engine API surface targets Linux hosts only;
// non-Linux builds must fail closed rather than construct a client that
// could silently no-op.
var ErrUnsupportedPlatform = errors.New("docker runtime unsupported on this platform")

// ErrorClass distinguishes why an Engine API call failed, so callers can map
// it onto AutoDeploy's own FailureClass without re-deriving transport
// details.
type ErrorClass uint8

const (
	// ErrClassUnknown is never returned by this package. Its presence makes
	// a zero-value EngineError detectably unclassified rather than silently
	// transient.
	ErrClassUnknown ErrorClass = iota
	// ErrClassTransient covers socket/dial/connection failures, context
	// deadline expiry, and Engine 5xx responses: retrying later may succeed.
	ErrClassTransient
	// ErrClassConfiguration covers Engine 4xx responses and invalid local
	// input (e.g. an empty socket path): the request itself is refused and
	// retrying it unchanged will not help.
	ErrClassConfiguration
	// ErrClassMalformed covers responses that parse as HTTP but not as the
	// shape this client expects, or that are internally inconsistent (a
	// ping that returns a non-OK body, a version response missing fields).
	ErrClassMalformed
)

func (c ErrorClass) String() string {
	switch c {
	case ErrClassTransient:
		return "transient"
	case ErrClassConfiguration:
		return "configuration"
	case ErrClassMalformed:
		return "malformed"
	default:
		return "unknown"
	}
}

// EngineError is the only error type the Engine API client returns for a
// failed call. Status is 0 when the failure occurred before an HTTP response
// was received (dial failure, timeout, malformed request).
//
// Error() and Err are operator-facing diagnostics only. A dial failure's Err
// embeds the literal Unix socket path from the Go runtime, which reveals
// host filesystem layout; callers must redact EngineError text before it can
// reach any tenant-visible surface (deployment logs/output, API responses).
// The path is kept, not dropped, because operators debugging a host need it.
//
// A non-2xx HTTP response's Err is the response BODY read verbatim
// (checkStatus in engine.go: `msg := strings.TrimSpace(string(body))`) --
// this is Docker's own daemon-authored text, not anything this client
// constructs, and it is embedded in Error()/Err unredacted. That matters
// for CreateContainer specifically (run.go): as of internal/agent/executor
// (Slice 9b Step 6b), its request body can carry runtime secret values in
// Env. Docker's daemon does not echo request-body content back in an error
// response today -- a 4xx from /containers/create names things like a
// missing image or an invalid host config shape, never the request JSON --
// so this is not a live leak, but it is an accepted, monitored gap rather
// than a proven-safe one: if a future Engine API version, a proxy in front
// of the socket, or an unexpected daemon error path ever changed that, a
// secret value could reach this same unredacted error text. Every call
// site that might log an EngineError (or any error wrapping one, via
// errors.As) must treat its text as untrusted and redact before logging or
// exposing it, exactly as the dial-failure case above already requires for
// the socket path.
type EngineError struct {
	Class  ErrorClass
	Op     string
	Status int
	Err    error
}

func (e *EngineError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("docker engine %s: %s (status %d): %v", e.Op, e.Class, e.Status, e.Err)
	}
	return fmt.Sprintf("docker engine %s: %s: %v", e.Op, e.Class, e.Err)
}

func (e *EngineError) Unwrap() error { return e.Err }
