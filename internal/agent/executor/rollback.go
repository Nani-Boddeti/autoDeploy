package executor

import (
	"context"
	"errors"
	"strings"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/agent/logship"
	"autodeploy/internal/deployment"
	"autodeploy/internal/queue"
	"autodeploy/internal/runtime/docker"
)

// Rollback (step 20D, owner decision D-R1): a rollback lease restarts the
// target release's retained container. No checkout, build, image work or
// container creation happens. The strategy comes from the live environment
// config carried on the lease (D-R2):
//
//   - blue_green: start the retained container, health gate it, activate; the
//     post-success drain of the old active release is the unchanged one.
//   - stop_then_start: activate first, then stop the old active release, start
//     the retained container and gate it inside the authorized window
//     (stsRelease with retained=true).
//
// A failure before authorization stops the retained container again (guarded,
// never removed). The target container is located by its exact identity labels;
// a missing or mismatched container is a permanent configuration failure.

// rollbackProgressPhases keep the deployment state machine valid (no skipped
// phase): the rollback has no fetch or build, but the states are only labels.
var rollbackProgressPhases = []deployment.Status{deployment.StatusFetching, deployment.StatusBuilding, deployment.StatusStarting}

func (e *Executor) executeRollback(ctx context.Context, lease agentclient.Lease, tracker *phaseTracker, progress *progressReporter, logs *logship.Attempt) agentclient.Outcome {
	if lease.RollbackTargetRuntimeInstanceID == "" || lease.ReleaseID != lease.RollbackTargetReleaseID {
		logs.Linef(logship.StreamPhase, "rollback: lease is malformed")
		return e.outcome(lease, false, queue.FailureClassCorruptState)
	}
	target := stopTarget{EnvironmentID: lease.EnvironmentID, ReleaseID: lease.ReleaseID, RuntimeInstanceID: lease.RollbackTargetRuntimeInstanceID}
	identity := docker.ReleaseResourceIdentity{EnvironmentID: lease.EnvironmentID, ReleaseID: lease.ReleaseID, RuntimeInstanceID: lease.RollbackTargetRuntimeInstanceID}

	for _, phase := range rollbackProgressPhases {
		if err := tracker.advance(ctx, progress, lease.AttemptID, lease.Fence, phase); err != nil {
			return e.outcome(lease, false, queue.FailureClassTransient)
		}
	}
	imageDigest, class, ok := e.locateRetained(ctx, target, logs)
	if !ok {
		return e.outcome(lease, false, class)
	}

	if lease.ReleaseStrategy == releaseStrategyStopThenStart {
		if err := tracker.advance(ctx, progress, lease.AttemptID, lease.Fence, deployment.StatusHealthChecking); err != nil {
			return e.outcome(lease, false, queue.FailureClassTransient)
		}
		return e.activateWith(ctx, lease, imageDigest, &stsRelease{e: e, lease: lease, identity: identity, retained: true})
	}

	inspect, res := e.startRetained(ctx, target)
	if res.Status != restartStarted {
		return e.outcome(lease, false, e.retainedStartFailure(res, logs))
	}
	out := func() agentclient.Outcome {
		if err := tracker.advance(ctx, progress, lease.AttemptID, lease.Fence, deployment.StatusHealthChecking); err != nil {
			return e.outcome(lease, false, queue.FailureClassTransient)
		}
		networkID, _, err := resolveNetworkAndIP(inspect)
		if err != nil {
			class := docker.ClassifyFailure(err)
			logs.Linef(logship.StreamHealth, "health gate: container network check failed (%s)", class)
			logs.Diagf(logship.StreamHealth, "health gate: container network check failed (%s): %v", class, err)
			return e.outcome(lease, false, class)
		}
		gate, err := e.docker.RunHealthGate(ctx, inspect.ID, networkID, lease.HealthPolicyRevision, docker.HealthGateTarget{
			Host: lease.HealthGateHost, Path: lease.HealthGatePath, Port: lease.ContainerPort, Scheme: lease.ContainerScheme,
		})
		if err != nil {
			class := docker.ClassifyFailure(err)
			logs.Linef(logship.StreamHealth, "health gate: error (%s)", class)
			logs.Diagf(logship.StreamHealth, "health gate: error (%s): %v", class, err)
			return e.outcome(lease, false, class)
		}
		if !gate.Success {
			class := docker.ClassifyHealthGateResult(gate)
			logs.Linef(logship.StreamHealth, "health gate: failed (reason=%s class=%s)", gate.Reason, class)
			logs.Diagf(logship.StreamHealth, "health gate: failed: last_probe=%q exit_code=%d oom_killed=%t", gate.LastProbe, gate.ExitCode, gate.OOMKilled)
			return e.outcome(lease, false, class)
		}
		logs.Linef(logship.StreamHealth, "health gate: passed")
		return e.activate(ctx, lease, imageDigest)
	}()
	if !out.HandedOff && !out.AuthorizationUnknown {
		e.stopFailedCandidate(ctx, lease, identity, logs)
	}
	return out
}

// locateRetained finds the target's retained container by its exact identity
// labels (exactly one match) and returns the image digest it was built from.
// ok=false carries the failure class; nothing was started.
func (e *Executor) locateRetained(ctx context.Context, t stopTarget, logs *logship.Attempt) (imageDigest string, class queue.FailureClass, ok bool) {
	matches, err := e.docker.ListContainersByLabels(ctx, map[string]string{
		docker.LabelManaged:           "true",
		docker.LabelKind:              docker.KindReleaseRuntimeContainer,
		docker.LabelEnvironmentID:     t.EnvironmentID,
		docker.LabelReleaseID:         t.ReleaseID,
		docker.LabelRuntimeInstanceID: t.RuntimeInstanceID,
	})
	if err != nil {
		class = classifyPhaseError(ctx, err)
		var ee *docker.EngineError
		if errors.As(err, &ee) {
			class = docker.ClassifyFailure(err)
		}
		logs.Linef(logship.StreamPhase, "rollback: container lookup failed (%s)", class)
		logs.Diagf(logship.StreamPhase, "rollback: container lookup failed (%s): %v", class, err)
		return "", class, false
	}
	if len(matches) != 1 {
		logs.Linef(logship.StreamPhase, "rollback: the retained release container was not found; it may have been removed")
		logs.Diagf(logship.StreamPhase, "rollback: %d containers match release %s runtime instance %s, want exactly 1; roll back by redeploying the commit instead", len(matches), t.ReleaseID, t.RuntimeInstanceID)
		return "", queue.FailureClassPermanentConfiguration, false
	}
	digest := matches[0].Labels[docker.LabelImageDigest]
	if !strings.HasPrefix(digest, imageDigestPrefix) {
		digest = imageDigestPrefix + digest
	}
	if _, valid := parseImageDigest(digest); !valid {
		logs.Linef(logship.StreamPhase, "rollback: the retained container has no valid image digest label")
		logs.Diagf(logship.StreamPhase, "rollback: container %s image digest label %q is not a sha256 digest", matches[0].ID, matches[0].Labels[docker.LabelImageDigest])
		return "", queue.FailureClassPermanentConfiguration, false
	}
	return digest, "", true
}

// startRetained starts the exact retained container (already running counts as
// started) and inspects it. The identity guard is restartPredecessor's.
func (e *Executor) startRetained(ctx context.Context, t stopTarget) (docker.ContainerInspect, restartResult) {
	res := restartPredecessor(ctx, e.docker, t)
	if res.Status != restartStarted {
		return docker.ContainerInspect{}, res
	}
	name, err := docker.ContainerName(t.RuntimeInstanceID)
	if err != nil {
		return docker.ContainerInspect{}, restartResult{Status: restartInvalid, Reason: "invalid runtime instance id", Err: err}
	}
	inspect, err := e.docker.InspectContainer(ctx, name)
	if err != nil {
		return docker.ContainerInspect{}, restartResult{Status: restartEngineError, Reason: "post-start inspect failed", Err: err}
	}
	return inspect, res
}

// retainedStartFailure logs a failed retained start and returns its class.
func (e *Executor) retainedStartFailure(res restartResult, logs *logship.Attempt) queue.FailureClass {
	class := queue.FailureClassTransient
	switch res.Status {
	case restartGone, restartMismatch, restartInvalid:
		class = queue.FailureClassPermanentConfiguration
	default:
		var ee *docker.EngineError
		if errors.As(res.Err, &ee) {
			class = docker.ClassifyFailure(res.Err)
		}
	}
	logs.Linef(logship.StreamPhase, "rollback: starting the retained container failed (%s)", class)
	logs.Diagf(logship.StreamPhase, "rollback: starting the retained container failed (%s): %s", class, res.text())
	return class
}
