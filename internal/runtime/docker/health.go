package docker

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// This file is Step 6a's health gate: ADR 0005's "Health-gate contract",
// pinned exactly as written there, with health_policy_revision fixed at 1
// (migrations 000014/000016 CHECK it can only be 1 today). None of the
// numbers below are configurable by a caller -- there is no field on
// HealthGateTarget for interval, timeout, deadline, or consecutive-success
// count, which is what makes "V1 pins the policy" a structural fact rather
// than a convention. Activation (route switching, Traefik) is a later
// slice: RunHealthGate only ever reports success/failure of the candidate
// gate itself and touches no routing state.

const (
	// healthGateConsecutiveSuccesses, healthGateProbeInterval,
	// healthGateProbeTimeout, and healthGateOverallDeadline are exactly
	// ADR 0005's default HTTP policy numbers.
	healthGateConsecutiveSuccesses = 3
	healthGateProbeInterval        = 2 * time.Second
	healthGateProbeTimeout         = 2 * time.Second
	healthGateOverallDeadline      = 60 * time.Second
	// healthGateMaxBodyBytes bounds the probe response body this package
	// will ever read before discarding it. The candidate is an arbitrary
	// application container, not the trusted Engine API, so this is
	// deliberately generous enough to let a normal health endpoint's body
	// drain without the connection stalling, while still bounding total
	// memory use per probe.
	healthGateMaxBodyBytes = 1 << 20 // 1 MiB
)

// ContainerState is the running/not-running classification the health gate
// checks before and around every probe. It intentionally has only two
// meaningfully distinct values for gating purposes (running vs. not); the
// third, ContainerStateUnknown, exists only so a zero value is detectably
// unset rather than silently "running".
type ContainerState int

const (
	ContainerStateUnknown ContainerState = iota
	ContainerStateRunning
	ContainerStateNotRunning
)

// HealthGateTarget is the exact, frozen probe target: host/path/port/scheme
// as configured for the environment (ADR 0005: "Exact configured container
// port, scheme, path, and Host header"). Host is the Host header value the
// probe sends; the TCP connection itself always goes to the container's own
// environment-network IP (resolved separately, see
// (*Client).RunReleaseHealthGate), never through any routing layer, so
// candidate health is provably independent of production route state.
type HealthGateTarget struct {
	Host   string
	Path   string
	Port   int
	Scheme string // "http" or "https"
}

func (t HealthGateTarget) validate() error {
	if t.Host == "" {
		return fmt.Errorf("%w: empty health gate host", ErrInvalidIdentifier)
	}
	if t.Path == "" || t.Path[0] != '/' {
		return fmt.Errorf("%w: health gate path must be absolute", ErrInvalidIdentifier)
	}
	if t.Port < 1 || t.Port > 65535 {
		return fmt.Errorf("%w: health gate port out of range", ErrInvalidIdentifier)
	}
	switch t.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("%w: unsupported health gate scheme %q", ErrInvalidIdentifier, t.Scheme)
	}
	return nil
}

// HealthProbeFunc performs exactly one probe attempt and reports whether it
// counted as a success. It is the injection seam that makes the gate loop
// testable without a real container: production wiring supplies
// newHTTPProbe's output, tests supply an httptest.Server-backed or entirely
// synthetic one.
type HealthProbeFunc func(ctx context.Context) (bool, error)

// ContainerStatus is the candidate's running state plus the evidence
// needed to classify a container that is NOT running. Exit code alone
// cannot separate "the kernel killed this under memory pressure" from
// "this application exited on its own", and the two classify differently:
// the first is a host condition that deserves a retry, the second is an
// application defect that does not. Docker reports OOMKilled on the same
// inspect call the gate already makes, so the evidence is free.
type ContainerStatus struct {
	State     ContainerState
	ExitCode  int
	OOMKilled bool
}

// ContainerStateFunc reports the candidate's current status. The other
// injection seam: production wiring supplies a function backed by
// (*Client).InspectContainer, tests supply a fake sequence of states.
type ContainerStateFunc func(ctx context.Context) (ContainerStatus, error)

// HealthGateFailureReason distinguishes why RunHealthGate did not report
// success. It is NOT informational only: ClassifyHealthGateResult maps
// each reason onto a different queue.FailureClass, and since only
// transient failures are ever retried (queue.FailureClass.Retryable), the
// reason decides whether a deployment gets another attempt at all.
type HealthGateFailureReason string

const (
	HealthGateFailureNone                HealthGateFailureReason = ""
	HealthGateFailureDeadlineExceeded    HealthGateFailureReason = "deadline_exceeded"
	HealthGateFailureContextCancelled    HealthGateFailureReason = "context_cancelled"
	HealthGateFailureContainerNotRunning HealthGateFailureReason = "container_not_running"
)

// HealthGateResult is RunHealthGate's outcome. Success is only ever true
// after exactly healthGateConsecutiveSuccesses consecutive successful
// probes with the container observed running throughout.
type HealthGateResult struct {
	Success bool
	Reason  HealthGateFailureReason
	// ExitCode and OOMKilled are meaningful only when Reason is
	// HealthGateFailureContainerNotRunning; they carry the evidence that
	// separates a host OOM kill from an application that exited on its
	// own, which classify differently.
	ExitCode  int
	OOMKilled bool
}

// runHealthGate is the gate loop's pure implementation, parameterized by
// interval/probeTimeout/deadline-bearing-context/consecutiveRequired so it
// is exercised in tests with millisecond-scale durations instead of the
// real 2s/2s/60s ADR numbers -- the loop logic under test is identical
// either way, only the clock is sped up. The exported production entry
// point (RunHealthGate) is a thin wrapper that hard-codes the ADR
// constants and can only ever call this with them; there is no exported way
// to run the gate with different numbers.
//
// ctx's own deadline (set by the caller to healthGateOverallDeadline in
// production) is the sole source of the 60s overall deadline: this
// function does not additionally track elapsed time, so "deadline
// exceeded" is reported precisely when ctx.Err() says so.
func runHealthGate(ctx context.Context, probe HealthProbeFunc, state ContainerStateFunc, interval, probeTimeout time.Duration, consecutiveRequired int) (HealthGateResult, error) {
	consecutive := 0
	for {
		if err := ctx.Err(); err != nil {
			return deadlineResult(err), nil
		}

		st, err := state(ctx)
		if err != nil {
			// The gate's own deadline can expire while an inspect call is
			// in flight, in which case the engine client reports a
			// transient context error. That is the same real condition as
			// the top-of-loop ctx.Err() check above, so it must classify
			// the same way -- otherwise an identical stuck candidate is
			// retried or permanently failed depending on which internal
			// timer happened to observe the expiry first.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return deadlineResult(ctxErr), nil
			}
			return HealthGateResult{}, err
		}
		if st.State != ContainerStateRunning {
			return HealthGateResult{
				Success:   false,
				Reason:    HealthGateFailureContainerNotRunning,
				ExitCode:  st.ExitCode,
				OOMKilled: st.OOMKilled,
			}, nil
		}

		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		ok, _ := probe(probeCtx)
		cancel()

		if ok {
			consecutive++
		} else {
			consecutive = 0
		}
		if consecutive >= consecutiveRequired {
			return HealthGateResult{Success: true}, nil
		}

		select {
		case <-ctx.Done():
			return deadlineResult(ctx.Err()), nil
		case <-time.After(interval):
		}
	}
}

func deadlineResult(err error) HealthGateResult {
	if errors.Is(err, context.Canceled) {
		return HealthGateResult{Success: false, Reason: HealthGateFailureContextCancelled}
	}
	return HealthGateResult{Success: false, Reason: HealthGateFailureDeadlineExceeded}
}

// RunHealthGate runs ADR 0005's default HTTP health gate against the
// container identified by containerID, resolving its IP on networkID via
// InspectContainer (the candidate is probed directly on its
// environment-network IP; release containers publish no host ports, so
// this is the only way to reach it) and probing target.
//
// health_policy_revision is asserted equal to 1 up front: V1 only ever
// pins revision 1 (see migrations 000014/000016), so any other value
// reaching this call is a caller bug, not a runtime condition to gate on.
func (c *Client) RunHealthGate(ctx context.Context, containerID, networkID string, healthPolicyRevision int, target HealthGateTarget) (HealthGateResult, error) {
	if healthPolicyRevision != 1 {
		return HealthGateResult{}, fmt.Errorf("%w: unsupported health_policy_revision %d", ErrInvalidIdentifier, healthPolicyRevision)
	}
	if err := target.validate(); err != nil {
		return HealthGateResult{}, err
	}

	ip, err := c.containerNetworkIP(ctx, containerID, networkID)
	if err != nil {
		return HealthGateResult{}, err
	}

	probe := newHTTPProbe(ip, target)
	state := func(ctx context.Context) (ContainerStatus, error) {
		return c.containerRunningState(ctx, containerID)
	}

	gateCtx, cancel := context.WithTimeout(ctx, healthGateOverallDeadline)
	defer cancel()
	return runHealthGate(gateCtx, probe, state, healthGateProbeInterval, healthGateProbeTimeout, healthGateConsecutiveSuccesses)
}

// containerNetworkIP resolves the container's IP address on the given
// network via InspectContainer. Failing to find the network in the
// container's own NetworkSettings is a malformed-response class error: a
// container this package created is always joined to exactly this network
// (see run.go), so its absence from the inspect result means the engine
// returned something internally inconsistent with what was asked for.
func (c *Client) containerNetworkIP(ctx context.Context, containerID, networkID string) (string, error) {
	inspect, err := c.InspectContainer(ctx, containerID)
	if err != nil {
		return "", err
	}
	for _, n := range inspect.NetworkSettings.Networks {
		if n.NetworkID == networkID {
			if n.IPAddress == "" {
				return "", &EngineError{Class: ErrClassMalformed, Op: "container-network-ip", Err: errors.New("container has no IP address on the environment network")}
			}
			return n.IPAddress, nil
		}
	}
	return "", &EngineError{Class: ErrClassMalformed, Op: "container-network-ip", Err: errors.New("container is not attached to the expected environment network")}
}

// containerRunningState maps InspectContainer's State.Running into the
// gate loop's ContainerState. A container that exited or that Docker
// reports in any other non-running status is ContainerStateNotRunning:
// ADR 0005 requires the container to remain running throughout, with no
// distinction carved out for "unhealthy" (this package applies no Docker
// HEALTHCHECK of its own, so that status never arises from a container it
// created) or "exited" -- both fail the gate identically.
func (c *Client) containerRunningState(ctx context.Context, containerID string) (ContainerStatus, error) {
	inspect, err := c.InspectContainer(ctx, containerID)
	if err != nil {
		return ContainerStatus{}, err
	}
	if inspect.State.Running {
		return ContainerStatus{State: ContainerStateRunning}, nil
	}
	return ContainerStatus{
		State:     ContainerStateNotRunning,
		ExitCode:  inspect.State.ExitCode,
		OOMKilled: inspect.State.OOMKilled,
	}, nil
}

// newHTTPProbe builds the default HealthProbeFunc: one HTTP(S) request
// straight to ip:target.Port, with target.Host as the Host header (never
// used for DNS resolution or connection -- the environment network has no
// external DNS, and this must not depend on production route state),
// target.Path as the request path, redirects disabled, and a 2xx-only
// success criterion. The response body is read up to
// healthGateMaxBodyBytes and discarded, never persisted, matching ADR
// 0005's "bounded response body that is discarded rather than persisted."
func newHTTPProbe(ip string, target HealthGateTarget) HealthProbeFunc {
	dialer := &net.Dialer{}
	addr := net.JoinHostPort(ip, strconv.Itoa(target.Port))
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},
		// The candidate's own certificate cannot be validated against the
		// configured Host from inside the environment network (there is
		// no CA relationship for an internal probe target); ADR 0005's
		// certificate/hostname validation requirement applies to the later
		// production data-plane check through Traefik, not this candidate
		// gate. This is a deliberate, narrower trust boundary: only
		// reachable from the agent's probe, never from outside the host.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	}
	client := &http.Client{
		Transport: transport,
		// Redirects OFF: ADR 0005 is explicit. Returning ErrUseLastResponse
		// makes the client return the redirect response itself instead of
		// following it, so a 3xx is evaluated (and rejected) as the
		// probe's own status rather than silently chased.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return func(ctx context.Context) (bool, error) {
		url := target.Scheme + "://" + target.Host + target.Path
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, err
		}
		req.Host = target.Host
		resp, err := client.Do(req)
		if err != nil {
			return false, nil
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, healthGateMaxBodyBytes))
		return resp.StatusCode >= 200 && resp.StatusCode < 300, nil
	}
}
