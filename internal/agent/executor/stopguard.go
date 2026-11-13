package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"autodeploy/internal/runtime/docker"
)

// The identity-guarded stop shared by the predecessor drainer and the
// executor's failed-candidate cleanup. It resolves the container by its
// deterministic AutoDeploy name, verifies the exact identity labels, stops it
// by the inspected container ID and waits until it is really stopped. It never
// removes or kills (DrainDocker has no such method) and never selects by an
// empty selector: every identity field is required.

// stopTarget is the exact identity of the container to stop.
type stopTarget struct {
	EnvironmentID     string
	ReleaseID         string
	RuntimeInstanceID string
	// ForbidReleaseID / ForbidRuntimeInstanceID, when set, refuse a container
	// whose labels name that release or instance (the drainer's "never the new
	// release" rule).
	ForbidReleaseID         string
	ForbidRuntimeInstanceID string
}

type stopStatus int

const (
	stopStopped        stopStatus = iota + 1 // stopped and confirmed
	stopAlreadyStopped                       // inspected as not running; nothing sent
	stopGone                                 // 404 at Step
	stopMismatch                             // identity check refused; Reason says why
	stopInvalid                              // the target cannot name a container
	stopEngineError                          // engine failure at Step; Err set
	stopInterrupted                          // ctx ended; nothing more known
)

type stopStep string

const (
	stepInspect stopStep = "inspect"
	stepLabels  stopStep = "labels"
	stepStop    stopStep = "stop"
	stepWait    stopStep = "wait"
)

type stopResult struct {
	Status stopStatus
	Step   stopStep
	Reason string
	Err    error
}

// guardedStop runs the identity-guarded stop. Callers own any further guard
// (the drainer's route-digest check runs before it).
func guardedStop(ctx context.Context, dk DrainDocker, t stopTarget, grace, waitTimeout, interval time.Duration) stopResult {
	if t.EnvironmentID == "" || t.ReleaseID == "" || t.RuntimeInstanceID == "" {
		return stopResult{Status: stopInvalid, Reason: "incomplete stop identity"}
	}
	name, err := docker.ContainerName(t.RuntimeInstanceID)
	if err != nil {
		return stopResult{Status: stopInvalid, Reason: "invalid runtime instance id", Err: err}
	}
	inspected, err := dk.InspectContainer(ctx, name)
	switch {
	case ctx.Err() != nil:
		return stopResult{Status: stopInterrupted}
	case errors.Is(err, docker.ErrContainerNotFound):
		return stopResult{Status: stopGone, Step: stepInspect, Err: err}
	case err != nil:
		return stopResult{Status: stopEngineError, Step: stepInspect, Err: err}
	}

	reason, err := checkStopIdentity(ctx, dk, t, inspected.ID)
	switch {
	case ctx.Err() != nil:
		return stopResult{Status: stopInterrupted}
	case err != nil:
		return stopResult{Status: stopEngineError, Step: stepLabels, Err: err}
	case reason != "":
		return stopResult{Status: stopMismatch, Reason: reason}
	}

	if !inspected.State.Running && !inspected.State.Restarting && !inspected.State.Paused && alreadyStopped(inspected.State.Status) {
		return stopResult{Status: stopAlreadyStopped}
	}

	// Pin the stop to the verified container's ID, not the name.
	target := inspected.ID
	if err := dk.StopContainer(ctx, target, grace); err != nil {
		switch {
		case ctx.Err() != nil:
			return stopResult{Status: stopInterrupted}
		case errors.Is(err, docker.ErrContainerNotFound):
			return stopResult{Status: stopGone, Step: stepStop, Err: err}
		}
		return stopResult{Status: stopEngineError, Step: stepStop, Err: err}
	}
	if _, err := dk.WaitStopped(ctx, target, waitTimeout, interval); err != nil {
		switch {
		case ctx.Err() != nil:
			return stopResult{Status: stopInterrupted}
		case errors.Is(err, docker.ErrContainerNotFound):
			return stopResult{Status: stopGone, Step: stepWait, Err: err}
		}
		return stopResult{Status: stopEngineError, Step: stepWait, Err: err}
	}
	return stopResult{Status: stopStopped}
}

// checkStopIdentity returns an empty reason when exactly one container matches
// the full identity labels, is the inspected one, and (when forbidden values
// are set) no label names them; otherwise a reason.
func checkStopIdentity(ctx context.Context, dk DrainDocker, t stopTarget, inspectedID string) (reason string, err error) {
	want := map[string]string{
		docker.LabelManaged:           "true",
		docker.LabelKind:              docker.KindReleaseRuntimeContainer,
		docker.LabelEnvironmentID:     t.EnvironmentID,
		docker.LabelReleaseID:         t.ReleaseID,
		docker.LabelRuntimeInstanceID: t.RuntimeInstanceID,
	}
	matches, err := dk.ListContainersByLabels(ctx, want)
	if err != nil {
		return "", err
	}
	if len(matches) != 1 {
		return fmt.Sprintf("%d containers match the identity labels, want exactly 1", len(matches)), nil
	}
	m := matches[0]
	if inspectedID == "" || m.ID != inspectedID {
		return "labelled container is not the container named for the identity", nil
	}
	if (t.ForbidReleaseID != "" && m.Labels[docker.LabelReleaseID] == t.ForbidReleaseID) ||
		(t.ForbidRuntimeInstanceID != "" && m.Labels[docker.LabelRuntimeInstanceID] == t.ForbidRuntimeInstanceID) {
		return "container labels name the new release or runtime instance", nil
	}
	for k, v := range want {
		if got, ok := m.Labels[k]; !ok || got != v {
			return "label " + k + " missing or different", nil
		}
	}
	return "", nil
}
