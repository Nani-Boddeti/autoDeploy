package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/github"
	"autodeploy/internal/runtime/docker"
)

// Failed-candidate cleanup (step 21B-2). The control plane proved the release
// was never activated and hands over one runtime instance id per deployment
// attempt; each may have left a stopped container. Per id, in order, exactly the
// retained rules apply (name + exact label set, exactly one match equal to the
// named container, guarded stop, NON-FORCE removal). Differences:
//
//   - the image-digest label is compared only when the claim names a digest; an
//     empty claim digest means "keep the image" (the server found it shared or
//     absent) and the image is never touched.
//   - any identity mismatch or engine error STOPS the claim: later ids are not
//     touched (removal is irreversible, so after anything unexpected the safest
//     action is none). Containers already removed stay removed and are listed in
//     the summary; the server keeps the cleanup open and redelivers it, and every
//     step is idempotent (removed ids are then simply absent).
//   - the image is removed (non-force, once) only after every id was removed or
//     absent, the claim names a digest, and the removed containers' inspected
//     image ids agree and equal "sha256:"+their config-digest label.

const maxFailedCandidateInstances = 64

func validFailedCandidateItem(it agentclient.CleanupItem) bool {
	if it.CleanupID <= 0 || !github.ValidOpaqueID(it.ReleaseID) || !github.ValidOpaqueID(it.EnvironmentID) || it.RuntimeInstanceID != "" {
		return false
	}
	if n := len(it.RuntimeInstanceIDs); n == 0 || n > maxFailedCandidateInstances {
		return false
	}
	seen := make(map[string]bool, len(it.RuntimeInstanceIDs))
	for _, id := range it.RuntimeInstanceIDs {
		if !validDecimalInstance(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	if it.ImageDigest == "" {
		return true
	}
	_, ok := parseImageDigest(imageDigestPrefix + it.ImageDigest)
	return ok && len(it.ImageDigest) == 64 && it.ImageDigest == strings.ToLower(it.ImageDigest)
}

func (c *Cleaner) removeFailedCandidate(ctx context.Context, it agentclient.CleanupItem) (result, class string, imageKept bool, summary string) {
	if !validFailedCandidateItem(it) {
		return "skipped", "identity", false, "claim carries an invalid identity"
	}
	var outcomes []string
	note := func(id, what string) { outcomes = append(outcomes, id+":"+what) }
	// stopped reports a non-success end of the claim; nothing after it is touched.
	stopped := func(result, class, id, what string) (string, string, bool, string) {
		note(id, what)
		return result, class, false, "stopped at instance " + id + " (" + what + "); later instances untouched; " + strings.Join(outcomes, " ")
	}
	var verifiedImage string // "" none yet; "-" ambiguous
	for _, id := range it.RuntimeInstanceIDs {
		name, err := docker.ContainerName(id)
		if err != nil {
			return stopped("skipped", "identity", id, "invalid id")
		}
		selector := map[string]string{
			docker.LabelManaged:           "true",
			docker.LabelKind:              docker.KindReleaseRuntimeContainer,
			docker.LabelEnvironmentID:     it.EnvironmentID,
			docker.LabelReleaseID:         it.ReleaseID,
			docker.LabelRuntimeInstanceID: id,
		}
		inspected, err := c.Docker.InspectContainer(ctx, name)
		switch {
		case ctx.Err() != nil:
			return stopped("failed", "other", id, "interrupted")
		case errors.Is(err, docker.ErrContainerNotFound):
			// Absent by name. Absent by labels too, or something is off.
			matches, lerr := c.Docker.ListContainersByLabels(ctx, selector)
			switch {
			case ctx.Err() != nil:
				return stopped("failed", "other", id, "interrupted")
			case lerr != nil:
				return stopped("failed", "engine", id, "label lookup failed: "+string(docker.ClassifyFailure(lerr)))
			case len(matches) != 0:
				return stopped("skipped", "identity", id, "absent by name but labels match another container; left alone")
			}
			note(id, "absent")
			continue
		case err != nil:
			return stopped("failed", "engine", id, "inspect failed: "+string(docker.ClassifyFailure(err)))
		}

		matches, err := c.Docker.ListContainersByLabels(ctx, selector)
		if err != nil {
			if ctx.Err() != nil {
				return stopped("failed", "other", id, "interrupted")
			}
			return stopped("failed", "engine", id, "label lookup failed: "+string(docker.ClassifyFailure(err)))
		}
		if len(matches) != 1 || matches[0].ID != inspected.ID || inspected.ID == "" {
			return stopped("skipped", "identity", id, fmt.Sprintf("%d containers match the identity labels and the named container differs; left alone", len(matches)))
		}
		labels := matches[0].Labels
		if it.ImageDigest != "" && strings.TrimPrefix(labels[docker.LabelImageDigest], imageDigestPrefix) != it.ImageDigest {
			return stopped("skipped", "identity", id, "image-digest label differs from the release; left alone")
		}

		how := "removed"
		if inspected.State.Running || inspected.State.Restarting || inspected.State.Paused {
			stop := guardedStop(ctx, c.Docker, stopTarget{EnvironmentID: it.EnvironmentID, ReleaseID: it.ReleaseID, RuntimeInstanceID: id},
				durOrDefault(c.StopGrace, cleanupStopGrace), durOrDefault(c.StopWait, cleanupStopWait), cleanupStopPoll)
			switch stop.Status {
			case stopStopped, stopAlreadyStopped:
				how = "stopped+removed"
			case stopGone:
				note(id, "vanished while stopping")
				verifiedImage = "-" // image id not verifiable: keep the image
				continue
			case stopMismatch:
				return stopped("skipped", "identity", id, "stop identity check refused: "+stop.Reason)
			default:
				return stopped("failed", "stop", id, "stopping failed ("+string(stop.Step)+")")
			}
		}
		switch err := c.Docker.RemoveContainer(ctx, inspected.ID); {
		case err == nil, errors.Is(err, docker.ErrContainerNotFound):
		case ctx.Err() != nil:
			return stopped("failed", "other", id, "interrupted")
		case errors.Is(err, docker.ErrContainerRunning):
			return stopped("failed", "stop", id, "engine refused removal (still running)")
		default:
			return stopped("failed", "engine", id, "remove failed: "+string(docker.ClassifyFailure(err)))
		}
		note(id, how)

		if cfg := strings.TrimPrefix(labels[docker.LabelConfigDigest], imageDigestPrefix); cfg != "" && inspected.Image == imageDigestPrefix+cfg {
			switch verifiedImage {
			case "":
				verifiedImage = inspected.Image
			case inspected.Image:
			default:
				verifiedImage = "-"
			}
		} else {
			verifiedImage = "-" // a removed container whose image id is unverified: keep the image
		}
	}

	summary = strings.Join(outcomes, " ")
	if it.ImageDigest == "" {
		return "removed", "", true, summary + "; image kept (not owned by this release)"
	}
	ref := ""
	if verifiedImage != "-" {
		ref = verifiedImage
	}
	kept, msg := c.removeImage(ctx, ref)
	return "removed", "", kept, summary + msg
}
