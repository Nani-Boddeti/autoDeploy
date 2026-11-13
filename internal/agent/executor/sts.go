package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/agent/logship"
	"autodeploy/internal/agent/state"
	"autodeploy/internal/deployment"
	"autodeploy/internal/queue"
	"autodeploy/internal/runtime/docker"
)

// The stop-then-start release strategy (step 20C, ADR 0005 amendment
// 2026-11-13). Sequence for lease.ReleaseStrategy == "stop_then_start":
//
//  1. checkout + build while the old release keeps serving (unchanged).
//  2. progress health_checking (an early label: it keeps the deployment state
//     machine starting -> health_checking -> activating valid without a
//     migration; the real gate runs later), Traefik preflight, server Activate.
//  3. persist the authorized record with PredecessorStopSent=true BEFORE the stop.
//  4. identity-guarded stop of the predecessor + WaitStopped. There is no drain
//     wait: V1 has no maintenance route, so nothing is switched ahead of the
//     stop, and the post-success drain is not armed (state.DrainFromActivation
//     owes nothing when PredecessorStopSent) because the predecessor is
//     already stopped.
//  5. create + start the candidate, run the health gate.
//  6. switchRoute -> verify -> probe -> report, exactly as blue/green.
//
// Failure after the stop: stop the candidate FIRST (running both is what this
// strategy exists to avoid), then restart the predecessor best effort, and
// report an activation FAILURE with an accurate summary. The server blocks the
// environment and the operator declares the outcome; the agent never does.
// A switch failure after the route file may already point at the candidate
// (on-disk digest equals the authorized one, or is unreadable) leaves both
// containers as found: tearing down a possibly-serving candidate would turn
// an unproven failure into a certain outage.

const (
	releaseStrategyStopThenStart = "stop_then_start"

	predecessorStopGrace      = 30 * time.Second
	predecessorStopWait       = 45 * time.Second
	predecessorStopTimeout    = 90 * time.Second
	predecessorRestartTimeout = 30 * time.Second
	// stsExtraTimeout widens the detached authorized-window deadline for the
	// stop + create + start + gate steps. Worst case before the switch:
	// predecessor stop 90s + candidate create/start (bounded by the Engine
	// client's own timeouts, budgeted 90s) + health gate 60s (the gate's
	// fixed overall deadline, healthGateOverallDeadline) = 4 minutes, leaving
	// the 4 minutes of slack for the switch window check below. Rollback does
	// NOT draw on this window: it runs on detached contexts (candidate stop
	// 30s + predecessor restart 30s). Additive, so ValidateActivationTimeout
	// and the blue/green timeout are unchanged.
	stsExtraTimeout = 8 * time.Minute
	// stsSwitchMargin is the slack required beyond ProbeTimeout +
	// verifyTimeout before the route may be written: with less window left a
	// switch that would succeed could be cut off, leaving the candidate
	// serving but the activation reported as a failure.
	stsSwitchMargin = 30 * time.Second
)

// RestartDocker is the Engine capability the stop-then-start release and
// recovery need: DrainDocker plus start. There is intentionally NO remove or
// kill method. *docker.Client satisfies it.
type RestartDocker interface {
	DrainDocker
	StartContainer(ctx context.Context, id string) error
}

type restartStatus int

const (
	restartStarted restartStatus = iota + 1
	restartMismatch
	restartInvalid
	restartGone
	restartEngineError
)

type restartResult struct {
	Status restartStatus
	Reason string
	Err    error
}

func (r restartResult) text() string {
	if r.Status == restartStarted {
		return "predecessor restart ok"
	}
	detail := r.Reason
	if r.Err != nil {
		detail = strings.TrimSpace(detail + " " + r.Err.Error())
	}
	return "predecessor restart failed (" + truncateUTF8(detail, 200) + ")"
}

// restartPredecessor starts the exact predecessor container again. Same
// identity guard as guardedStop (every label must match, never the candidate
// named by the Forbid fields); the start is pinned to the inspected container
// ID and a final inspect confirms it is running.
func restartPredecessor(ctx context.Context, dk RestartDocker, t stopTarget) restartResult {
	if t.EnvironmentID == "" || t.ReleaseID == "" || t.RuntimeInstanceID == "" {
		return restartResult{Status: restartInvalid, Reason: "incomplete restart identity"}
	}
	name, err := docker.ContainerName(t.RuntimeInstanceID)
	if err != nil {
		return restartResult{Status: restartInvalid, Reason: "invalid runtime instance id", Err: err}
	}
	inspected, err := dk.InspectContainer(ctx, name)
	switch {
	case errors.Is(err, docker.ErrContainerNotFound):
		return restartResult{Status: restartGone, Reason: "predecessor container not found", Err: err}
	case err != nil:
		return restartResult{Status: restartEngineError, Reason: "inspect failed", Err: err}
	}
	reason, err := checkStopIdentity(ctx, dk, t, inspected.ID)
	switch {
	case err != nil:
		return restartResult{Status: restartEngineError, Reason: "label lookup failed", Err: err}
	case reason != "":
		return restartResult{Status: restartMismatch, Reason: reason}
	}
	if inspected.State.Running {
		return restartResult{Status: restartStarted} // nothing to restart
	}
	if err := dk.StartContainer(ctx, inspected.ID); err != nil {
		return restartResult{Status: restartEngineError, Reason: "start failed", Err: err}
	}
	after, err := dk.InspectContainer(ctx, inspected.ID)
	switch {
	case err != nil:
		return restartResult{Status: restartEngineError, Reason: "post-start inspect failed", Err: err}
	case !after.State.Running:
		return restartResult{Status: restartEngineError, Reason: "predecessor not running after start"}
	}
	return restartResult{Status: restartStarted}
}

// executeStopThenStart runs steps 2-6 of the sequence above. The candidate
// container does not exist yet, so a failure before authorization has nothing
// to stop.
func (e *Executor) executeStopThenStart(ctx context.Context, lease agentclient.Lease, identity docker.ReleaseResourceIdentity, env []docker.ContainerEnv, imageDigest string, tracker *phaseTracker, progress *progressReporter, logs *logship.Attempt) agentclient.Outcome {
	if err := tracker.advance(ctx, progress, lease.AttemptID, lease.Fence, deployment.StatusHealthChecking); err != nil {
		return e.outcome(lease, false, queue.FailureClassTransient)
	}
	return e.activateWith(ctx, lease, imageDigest, &stsRelease{e: e, lease: lease, identity: identity, env: env})
}

// stsRelease carries the stop-then-start steps run inside activateWith.
type stsRelease struct {
	e        *Executor
	lease    agentclient.Lease
	identity docker.ReleaseResourceIdentity
	env      []docker.ContainerEnv
	// retained (rollback, step 20D): the candidate is the target's existing
	// container, started rather than created.
	retained bool

	// stopSent: the predecessor may be down and a restart is owed on failure.
	// Set when a stop was issued and ended stopped or unknown; not when it was
	// already stopped, absent or refused.
	stopSent bool
}

func (s *stsRelease) failure(ctx context.Context, rec state.PendingActivation, reason string) string {
	a := s.e.activation
	return a.failureSummary("stop-then-start", reason, routeDigestString(rec.RouteDigest), rec.EnvironmentID)
}

// failRolledBack rolls back and reports a failure. The rollback outcome comes
// FIRST in the reason so summary truncation can only ever cut the detail.
func (s *stsRelease) failRolledBack(ctx context.Context, rec state.PendingActivation, detail string) string {
	return s.failure(ctx, rec, s.rollback(ctx, rec)+"; "+detail)
}

// beforeSwitch runs stop, create/start and the health gate. It returns "" on
// success or a failure summary (after rolling back).
func (s *stsRelease) beforeSwitch(ctx context.Context, rec *state.PendingActivation) string {
	e, a, lease := s.e, s.e.activation, s.lease
	logs := logship.FromContext(ctx)

	if rec.HasPredecessor() {
		if rec.PredecessorRuntimeInstanceID == s.identity.RuntimeInstanceID || rec.PredecessorReleaseID == lease.ReleaseID {
			logs.Linef(logship.StreamActivation, "release: predecessor equals the candidate; refusing to stop")
			return s.failure(ctx, *rec, "predecessor identity equals the candidate; nothing stopped or started")
		}
		// Durable intent BEFORE the stop. If this save fails nothing was
		// stopped, so the in-memory flag stays set: a later save may flip
		// false -> true on disk, while resetting it could contradict an
		// ambiguous-but-landed write (ErrActivationImmutable).
		rec.PredecessorStopSent = true
		if err := a.saveWithRetry(*rec); err != nil {
			logs.Linef(logship.StreamActivation, "release: cannot persist the stop intent; predecessor not stopped")
			return s.failRolledBack(ctx, *rec, "could not persist the predecessor stop intent; predecessor not stopped: "+err.Error())
		}
		logs.Linef(logship.StreamActivation, "release: stopping predecessor")
		// The stop deliberately stays bound to the authorized window (ctx): if
		// the window is already gone no new stop should start, and a stop cut
		// off mid-way ends "unknown", which sets stopSent so rollback restarts
		// the predecessor on a detached context.
		sctx, cancel := context.WithTimeout(ctx, predecessorStopTimeout)
		res := guardedStop(sctx, e.docker, stopTarget{
			EnvironmentID:           lease.EnvironmentID,
			ReleaseID:               rec.PredecessorReleaseID,
			RuntimeInstanceID:       rec.PredecessorRuntimeInstanceID,
			ForbidReleaseID:         lease.ReleaseID,
			ForbidRuntimeInstanceID: s.identity.RuntimeInstanceID,
		}, predecessorStopGrace, predecessorStopWait, time.Second)
		cancel()
		switch res.Status {
		case stopStopped:
			s.stopSent = true
			logs.Linef(logship.StreamActivation, "release: predecessor stopped")
		case stopAlreadyStopped, stopGone:
			logs.Linef(logship.StreamActivation, "release: predecessor was not running; nothing stopped")
		case stopMismatch, stopInvalid:
			logs.Linef(logship.StreamActivation, "release: predecessor stop refused by the identity guard")
			return s.failRolledBack(ctx, *rec, fmt.Sprintf("predecessor stop refused: %s; predecessor not stopped", res.Reason))
		default:
			s.stopSent = true // outcome unknown: it may be down
			logs.Linef(logship.StreamActivation, "release: predecessor stop failed")
			return s.failRolledBack(ctx, *rec, fmt.Sprintf("predecessor stop failed at %q (candidate never started): %v", res.Step, res.Err))
		}
	}

	var inspect docker.ContainerInspect
	var err error
	if s.retained {
		var res restartResult
		inspect, res = e.startRetained(ctx, stopTarget{EnvironmentID: lease.EnvironmentID, ReleaseID: lease.ReleaseID, RuntimeInstanceID: s.identity.RuntimeInstanceID})
		if res.Status != restartStarted {
			class := e.retainedStartFailure(res, logs)
			logs.Linef(logship.StreamActivation, "release: retained candidate start failed (%s)", class)
			return s.failRolledBack(ctx, *rec, fmt.Sprintf("predecessor stopped; retained candidate start failed (%s)", class))
		}
	} else {
		inspect, err = e.docker.EnsureReleaseRuntimeContainer(ctx, docker.RunContainerRequest{
			Identity:      s.identity,
			CPUMillis:     lease.CPUMillis,
			MemoryBytes:   lease.MemoryBytes,
			RestartPolicy: lease.RestartPolicy,
			Env:           s.env,
		}, e.network)
		if err != nil {
			class := docker.ClassifyFailure(err)
			logs.Linef(logship.StreamActivation, "release: candidate start failed (%s)", class)
			return s.failRolledBack(ctx, *rec, fmt.Sprintf("predecessor stopped; candidate start failed (%s)", class))
		}
	}
	networkID, _, err := resolveNetworkAndIP(inspect)
	if err != nil {
		class := docker.ClassifyFailure(err)
		logs.Linef(logship.StreamHealth, "health gate: container network check failed (%s)", class)
		return s.failRolledBack(ctx, *rec, fmt.Sprintf("predecessor stopped; candidate network check failed (%s)", class))
	}
	gate, err := e.docker.RunHealthGate(ctx, inspect.ID, networkID, lease.HealthPolicyRevision, docker.HealthGateTarget{
		Host: lease.HealthGateHost, Path: lease.HealthGatePath, Port: lease.ContainerPort, Scheme: lease.ContainerScheme,
	})
	switch {
	case err != nil:
		class := docker.ClassifyFailure(err)
		logs.Linef(logship.StreamHealth, "health gate: error (%s)", class)
		return s.failRolledBack(ctx, *rec, fmt.Sprintf("predecessor stopped; candidate health gate errored (%s)", class))
	case !gate.Success:
		logs.Linef(logship.StreamHealth, "health gate: failed (reason=%s)", gate.Reason)
		return s.failRolledBack(ctx, *rec, fmt.Sprintf("predecessor stopped; candidate failed health gate: reason=%s last_probe=%q", gate.Reason, gate.LastProbe))
	}
	logs.Linef(logship.StreamHealth, "health gate: passed")
	// Do not write the route with too little window left to verify and probe
	// it: a switch cut off by the deadline leaves the candidate serving but
	// reported failed (operator-blocking). Roll back instead.
	if deadline, ok := ctx.Deadline(); ok {
		need := a.cfg.ProbeTimeout + a.verifyTimeout + stsSwitchMargin
		if left := time.Until(deadline); left < need {
			logs.Linef(logship.StreamActivation, "release: authorized window too short to switch; rolling back")
			return s.failRolledBack(ctx, *rec, fmt.Sprintf("authorized window too short to switch the route (%s left, need %s); route not changed", left.Round(time.Second), need))
		}
	}
	return ""
}

// switchFailurePrefix decides, at the moment a switch step fails, whether to
// roll the containers back (see the package comment) and returns the text that
// leads the failure reason. It is the reasonPrefix hook of switchRoute, so the
// outcome is part of the bounded reason rather than prepended to a finished
// summary (which could cut the digest evidence).
func (s *stsRelease) switchFailurePrefix(ctx context.Context, rec state.PendingActivation) string {
	a := s.e.activation
	authorized := routeDigestString(rec.RouteDigest)
	onDisk, exists, err := a.readDigest(a.cfg.RouteDir, rec.EnvironmentID)
	if err != nil || (exists && onDisk == authorized) {
		return "route switch failed; the route may point at the candidate, so the candidate and predecessor were left as found"
	}
	return s.rollback(ctx, rec) + "; route switch failed before the route changed"
}

// rollback stops the candidate and, only if that is confirmed, restarts the
// predecessor when this attempt stopped it. It returns the summary text.
func (s *stsRelease) rollback(ctx context.Context, rec state.PendingActivation) string {
	logs := logship.FromContext(ctx)
	candidateStopped := s.e.stopFailedCandidate(ctx, s.lease, s.identity, logs)
	cand := "candidate stopped"
	if !candidateStopped {
		cand = "candidate could NOT be stopped"
	}
	switch {
	case !rec.HasPredecessor():
		return "predecessor restart not attempted (no predecessor); " + cand
	case !s.stopSent:
		return "predecessor restart not attempted (not stopped by this attempt); " + cand
	case !candidateStopped:
		return "predecessor restart not attempted (running both is unsafe; operator action required); " + cand
	}
	// Detached: an expired authorized window must not make the restart fail
	// instantly and leave the predecessor down.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), predecessorRestartTimeout)
	defer cancel()
	res := restartPredecessor(rctx, s.e.docker, stopTarget{
		EnvironmentID:           s.lease.EnvironmentID,
		ReleaseID:               rec.PredecessorReleaseID,
		RuntimeInstanceID:       rec.PredecessorRuntimeInstanceID,
		ForbidReleaseID:         s.lease.ReleaseID,
		ForbidRuntimeInstanceID: s.identity.RuntimeInstanceID,
	})
	if res.Status == restartStarted {
		logs.Linef(logship.StreamActivation, "release: predecessor restarted")
	} else {
		logs.Linef(logship.StreamActivation, "release: predecessor restart failed")
		s.e.logger.Printf("agent executor ERROR attempt %d: predecessor %s restart failed: %s", s.lease.AttemptID, rec.PredecessorRuntimeInstanceID, res.text())
	}
	return res.text() + "; " + cand
}

// recoverStoppedPredecessor is the recovery counterpart of rollback for a
// stranded authorized record with PredecessorStopSent: stop the candidate
// (which may be running after a crash past the start) and restart the
// predecessor. Callers have already established the route does not point at
// the candidate. It returns the summary text.
func recoverStoppedPredecessor(dk RestartDocker, rec state.PendingActivation) string {
	if dk == nil {
		return "predecessor restart not attempted (no docker access in recovery); operator action required"
	}
	sctx, cancel := context.WithTimeout(context.Background(), candidateStopTimeout)
	res := guardedStop(sctx, dk, stopTarget{
		EnvironmentID: rec.EnvironmentID, ReleaseID: rec.ReleaseID, RuntimeInstanceID: rec.BackendRuntimeInstanceID,
	}, candidateStopGrace, candidateStopWait, time.Second)
	cancel()
	switch res.Status {
	case stopStopped, stopAlreadyStopped, stopGone:
	default:
		return "predecessor restart not attempted (running both is unsafe; operator action required); candidate could NOT be stopped"
	}
	rctx, cancel := context.WithTimeout(context.Background(), predecessorRestartTimeout)
	defer cancel()
	return restartPredecessor(rctx, dk, stopTarget{
		EnvironmentID: rec.EnvironmentID, ReleaseID: rec.PredecessorReleaseID, RuntimeInstanceID: rec.PredecessorRuntimeInstanceID,
		ForbidReleaseID: rec.ReleaseID, ForbidRuntimeInstanceID: rec.BackendRuntimeInstanceID,
	}).text() + "; candidate stopped"
}
