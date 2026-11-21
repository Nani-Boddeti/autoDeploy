// Package checkoutquota exposes the intentionally small privileged checkout
// quota boundary. Callers can request only bounded allocation and release for
// a descriptor; paths, devices, and project identifiers are never RPC input.
package checkoutquota

import (
	"errors"
	"fmt"
	"time"
)

// Stale-allocation reclaim knobs (23-H, owner decision D3). An allocation that
// stays `active` this long has lost its holder: the longest legitimate hold is
// checkout (120s spawn limit) plus one build (--build-timeout, at most 2h,
// cmd/agent/main.go). The floor is that 2h + 2m plus margin, rounded up to 3h;
// the default (6h) sits far above it.
const (
	DefaultReclaimTTL    = 6 * time.Hour
	MinReclaimTTL        = 3 * time.Hour
	DefaultSweepInterval = 10 * time.Minute
	MinSweepInterval     = time.Minute
)

// ValidateSweep checks explicit reclaim settings. Zero values mean "use the
// default" only in Config; here both must be concrete.
func ValidateSweep(ttl, interval time.Duration) error {
	if ttl < MinReclaimTTL {
		return fmt.Errorf("%w: reclaim ttl must be at least %s", ErrInvalid, MinReclaimTTL)
	}
	if interval < MinSweepInterval {
		return fmt.Errorf("%w: sweep interval must be at least %s", ErrInvalid, MinSweepInterval)
	}
	return nil
}

var (
	ErrUnavailable = errors.New("checkout quota service unavailable")
	ErrDenied      = errors.New("checkout quota service denied request")
	ErrInvalid     = errors.New("checkout quota service invalid request")
	ErrRetryable   = errors.New("checkout quota service cleanup pending")
	ErrCompleted   = errors.New("checkout quota service cleanup already completed")
)

type Limits struct {
	Bytes  uint64
	Inodes uint64
}

// Config is fixed at service startup. StatePath is operator controlled and
// records a mount-bound, monotonic project-ID counter.
type Config struct {
	CheckoutRoot string
	SocketPath   string
	StatePath    string
	AgentUID     uint32
	// AgentGID is the configured agent's primary group. The socket remains
	// root-owned and grants connect permission only to this group.
	AgentGID uint32
	// CheckoutUID and CheckoutGID are the fixed, non-root identity that owns
	// active workspaces. They are intentionally separate from the agent socket
	// identity: the checkout process must not be able to connect to helpers.
	CheckoutUID uint32
	CheckoutGID uint32
	MaxBytes    uint64
	MaxInodes   uint64
	// ReclaimTTL is the age at which an `active` allocation is reclaimed by the
	// periodic sweep; SweepInterval is the sweep period. Zero selects the
	// defaults; explicit values are validated by ValidateSweep.
	ReclaimTTL       time.Duration
	SweepInterval    time.Duration
	Initialize       bool   // one-time, explicit operator ledger initialization
	InitialProjectID uint32 // required only with Initialize; never defaults to one
}

// Client talks only to the fixed local quota helper. It never falls back to a
// path based allocation or cleanup operation.
type Client struct {
	SocketPath string
	// AgentGID must match the group configured on the root-owned service socket.
	AgentGID uint32
}

// Allocation is an acknowledged workspace lease. Workspace is owned by the
// caller and must be closed after ReleaseAndCleanup returns.
type Allocation struct {
	Workspace int
	Handle    [16]byte
}

// IndeterminateAllocationError means the helper may have committed the final
// acknowledgement but its reply was lost. Allocation remains an owned
// capability and must be passed to ResolveAllocation or released by the caller.
type IndeterminateAllocationError struct {
	Allocation Allocation
	Err        error
}

func (e *IndeterminateAllocationError) Error() string {
	return "checkout quota allocation outcome indeterminate"
}
func (e *IndeterminateAllocationError) Unwrap() error { return e.Err }

// blocksForBytes computes the XFS 512-byte quota ceiling without addition
// overflow. A zero request is never an enforceable allocation.
func blocksForBytes(bytes uint64) (uint64, bool) {
	if bytes == 0 {
		return 0, false
	}
	blocks := bytes / 512
	if bytes%512 != 0 {
		blocks++
	}
	return blocks, blocks != 0
}

func (c Config) validLimits(l Limits) bool {
	_, ok := blocksForBytes(l.Bytes)
	return ok && l.Inodes > 0 && l.Bytes <= c.MaxBytes && l.Inodes <= c.MaxInodes
}
