package executor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/github"
	"autodeploy/internal/runtime/docker"
)

// The retention Cleaner (step 20E, owner decisions B-D5/B-D6). The control
// plane decides WHAT may be removed: ClaimCleanups moves a retained release to
// 'removing' under the environment slot lock after proving it is neither
// active, a rollback target, nor owned by an unresolved activation. The agent
// only removes exactly what a claim names, and only when the container carries
// the exact identity labels:
//
//   - container: located by name and by the full label set (managed, kind,
//     environment, release, runtime instance) with exactly one match that is
//     the named container, and whose image-digest label equals the claim's. Any
//     disagreement is a "skipped" report: nothing is touched.
//   - running: stopped first with the identity-guarded guardedStop (the server
//     already proved the release is not active, so it must not be serving).
//   - removal: RemoveContainer and RemoveImage are NON-FORCE; the Engine refuses
//     a running container or an image another container still uses. An image in
//     use is not an error: the container part succeeded and image_kept is
//     reported.
//   - a missing container is already removed (success).
//
// Reports are idempotent per cleanup id. There is no agent-local pending-report
// file: the server hands an unreported cleanup out again on the next claim and
// every step here is idempotent, so a lost report is simply redone, and the
// server never believes a removal happened that did not (it only marks the
// release removed on an explicit "removed" report).

const (
	defaultCleanupInterval = 10 * time.Minute
	cleanupClaimBatch      = 4
	cleanupStopGrace       = 30 * time.Second
	cleanupStopWait        = 60 * time.Second
	cleanupStopPoll        = time.Second
)

// CleanupDocker is the Engine capability the Cleaner needs: the drainer's
// inspect/list/stop/wait plus the two non-force removals.
type CleanupDocker interface {
	DrainDocker
	RemoveContainer(ctx context.Context, id string) error
	RemoveImage(ctx context.Context, ref string) error
}

// CleanupControlPlane is the control-plane side of the cleanup protocol.
// *agentclient.Client satisfies it.
type CleanupControlPlane interface {
	ClaimCleanups(ctx context.Context, limit int) ([]agentclient.CleanupItem, error)
	ReportCleanupResult(ctx context.Context, cleanupID int64, result, errorClass string, imageKept bool, summary string) error
}

// Cleaner removes claimed retained releases. Run must not be called
// concurrently with itself.
type Cleaner struct {
	Docker       CleanupDocker
	ControlPlane CleanupControlPlane
	Logger       *log.Logger
	// Interval between passes; zero uses 10 minutes. A pass also runs once at
	// start (the agent starts the Cleaner after recovery).
	Interval time.Duration
	// StopGrace and StopWait bound stopping a running container; zero uses
	// 30s and 60s.
	StopGrace, StopWait time.Duration
}

// Validate reports missing dependencies.
func (c *Cleaner) Validate() error {
	if c == nil || c.Docker == nil || c.ControlPlane == nil {
		return errors.New("incomplete retention cleaner configuration")
	}
	return nil
}

func (c *Cleaner) logger() *log.Logger {
	if c.Logger == nil {
		return log.Default()
	}
	return c.Logger
}

// Run passes once immediately and then every Interval until ctx ends.
func (c *Cleaner) Run(ctx context.Context) error {
	if err := c.Validate(); err != nil {
		return err
	}
	interval := durOrDefault(c.Interval, defaultCleanupInterval)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		c.Pass(ctx)
		timer.Reset(interval)
	}
}

// Pass claims and processes one batch.
func (c *Cleaner) Pass(ctx context.Context) {
	items, err := c.ControlPlane.ClaimCleanups(ctx, cleanupClaimBatch)
	if err != nil {
		if ctx.Err() == nil {
			c.logger().Printf("agent cleaner: claim failed: %v", err)
		}
		return
	}
	for _, item := range items {
		if ctx.Err() != nil {
			return
		}
		result, class, kept, summary := c.remove(ctx, item)
		if err := c.ControlPlane.ReportCleanupResult(ctx, item.CleanupID, result, class, kept, resultSummary(summary)); err != nil {
			if ctx.Err() != nil {
				return
			}
			// Not lost: the server still has the cleanup open and hands it out
			// again; the next pass redoes the (idempotent) removal and reports.
			c.logger().Printf("agent cleaner: report for cleanup %d failed (it will be handed out again): %v", item.CleanupID, err)
			continue
		}
		c.logger().Printf("agent cleaner: release %s cleanup %d: %s", item.ReleaseID, item.CleanupID, result)
	}
}

func validCleanupItem(it agentclient.CleanupItem) bool {
	if it.CleanupID <= 0 || !github.ValidOpaqueID(it.ReleaseID) || !github.ValidOpaqueID(it.EnvironmentID) || !validDecimalInstance(it.RuntimeInstanceID) {
		return false
	}
	_, ok := parseImageDigest(imageDigestPrefix + it.ImageDigest)
	return ok && len(it.ImageDigest) == 64 && it.ImageDigest == strings.ToLower(it.ImageDigest)
}

func validDecimalInstance(s string) bool {
	if s == "" || len(s) > 19 || s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// remove performs one release's removal and returns the report.
func (c *Cleaner) remove(ctx context.Context, it agentclient.CleanupItem) (result, class string, imageKept bool, summary string) {
	if !validCleanupItem(it) {
		return "skipped", "identity", false, "claim carries an invalid identity"
	}
	failed := func(class, format string, args ...any) (string, string, bool, string) {
		return "failed", class, false, fmt.Sprintf(format, args...)
	}
	name, err := docker.ContainerName(it.RuntimeInstanceID)
	if err != nil {
		return "skipped", "identity", false, "invalid runtime instance id"
	}
	inspected, err := c.Docker.InspectContainer(ctx, name)
	switch {
	case ctx.Err() != nil:
		return failed("other", "interrupted")
	case errors.Is(err, docker.ErrContainerNotFound):
		// Already removed (an earlier pass whose report was lost, or removed by
		// an operator). The image id cannot be verified without the container's
		// labels, so no image is removed: report image_kept.
		return "removed", "", true, "container already absent; image kept (not verifiable)"
	case err != nil:
		return failed("engine", "container inspect failed: %s", docker.ClassifyFailure(err))
	}

	// Exact identity: one container with every label, the inspected one, and the
	// release's image digest.
	matches, err := c.Docker.ListContainersByLabels(ctx, map[string]string{
		docker.LabelManaged:           "true",
		docker.LabelKind:              docker.KindReleaseRuntimeContainer,
		docker.LabelEnvironmentID:     it.EnvironmentID,
		docker.LabelReleaseID:         it.ReleaseID,
		docker.LabelRuntimeInstanceID: it.RuntimeInstanceID,
	})
	if err != nil {
		if ctx.Err() != nil {
			return failed("other", "interrupted")
		}
		return failed("engine", "container label lookup failed: %s", docker.ClassifyFailure(err))
	}
	if len(matches) != 1 || matches[0].ID != inspected.ID || inspected.ID == "" {
		return "skipped", "identity", false, fmt.Sprintf("%d containers match the identity labels and the named container differs; left alone", len(matches))
	}
	labels := matches[0].Labels
	if strings.TrimPrefix(labels[docker.LabelImageDigest], imageDigestPrefix) != it.ImageDigest {
		return "skipped", "identity", false, "container image-digest label differs from the release; left alone"
	}

	if inspected.State.Running || inspected.State.Restarting || inspected.State.Paused {
		stop := guardedStop(ctx, c.Docker, stopTarget{EnvironmentID: it.EnvironmentID, ReleaseID: it.ReleaseID, RuntimeInstanceID: it.RuntimeInstanceID},
			durOrDefault(c.StopGrace, cleanupStopGrace), durOrDefault(c.StopWait, cleanupStopWait), cleanupStopPoll)
		switch stop.Status {
		case stopStopped, stopAlreadyStopped:
		case stopGone:
			return "removed", "", true, "container vanished while stopping; image kept (not verifiable)"
		case stopMismatch:
			return "skipped", "identity", false, "stop identity check refused: " + stop.Reason
		default:
			return failed("stop", "stopping the running container failed (%s)", stop.Step)
		}
	}

	switch err := c.Docker.RemoveContainer(ctx, inspected.ID); {
	case err == nil, errors.Is(err, docker.ErrContainerNotFound):
	case ctx.Err() != nil:
		return failed("other", "interrupted")
	case errors.Is(err, docker.ErrContainerRunning):
		return failed("stop", "engine refused to remove the container (still running)")
	default:
		return failed("engine", "container remove failed: %s", docker.ClassifyFailure(err))
	}

	// The image to try is the container's own image id, only when it is exactly
	// the config digest this container's labels name.
	ref := ""
	if cfg := strings.TrimPrefix(labels[docker.LabelConfigDigest], imageDigestPrefix); cfg != "" && inspected.Image == imageDigestPrefix+cfg {
		ref = inspected.Image
	}
	kept, msg := c.removeImage(ctx, ref)
	return "removed", "", kept, "container removed" + msg
}

// removeImage removes ref without force. "In use" and any failure keep the
// image; neither is a failure of the container removal that already happened.
func (c *Cleaner) removeImage(ctx context.Context, ref string) (kept bool, msg string) {
	if ref == "" {
		return true, "; image kept (no verified image id)"
	}
	switch err := c.Docker.RemoveImage(ctx, ref); {
	case err == nil:
		return false, "; image removed"
	case errors.Is(err, docker.ErrImageNotFound):
		return false, "; image already absent"
	case errors.Is(err, docker.ErrImageInUse):
		return true, "; image kept (in use)"
	default:
		class := docker.ClassifyFailure(err)
		c.logger().Printf("agent cleaner: image %s removal failed (%s), image kept: %v", ref, class, err)
		return true, "; image kept (remove failed: " + string(class) + ")"
	}
}
