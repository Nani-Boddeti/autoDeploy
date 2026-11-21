package agentclient

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"autodeploy/internal/agent/state"
	"autodeploy/internal/logging"
	"autodeploy/internal/queue"
)

// Slice 23-G (owner decision D2). A restarted agent finds the attempts it lost
// and acknowledges only those that provably left no traffic-affecting state:
// during checkout or build it reports the attempt failed/transient so it closes
// cleanly and the job retries under the normal limits. What may remain from
// those phases is harmless to traffic: build cache and images (cleaned by
// retention), the checkout workspace (reclaimed by the quota service) and the
// shared BuildKit builder (idempotent, carries no attempt identity). From starting onward it
// only logs and leaves the attempt to lease expiry, which blocks per 23-F. It
// never declares an activation outcome (owner decision D-B): activation
// recovery owns attempts with a durable activation record.

// lostAttemptRetryBackoff is the wait before the 2nd and 3rd Complete try
// (three tries in total). A var so tests can shorten it.
var lostAttemptRetryBackoff = []time.Duration{time.Second, 2 * time.Second}

type phaseRecorderKey struct{}

// PhaseRecorder durably records the phase the executor is about to begin. A
// non-nil error means the record could not be written and the phase must not
// start: the record may never claim an earlier phase than the work in progress.
type PhaseRecorder func(phase string) error

// WithPhaseRecorder attaches rec to ctx for the executor.
func WithPhaseRecorder(ctx context.Context, rec PhaseRecorder) context.Context {
	return context.WithValue(ctx, phaseRecorderKey{}, rec)
}

// PhaseRecorderFrom returns the recorder attached to ctx, or nil.
func PhaseRecorderFrom(ctx context.Context) PhaseRecorder {
	rec, _ := ctx.Value(phaseRecorderKey{}).(PhaseRecorder)
	return rec
}

// LostAttemptRestartOperation derives the Complete operation id and result
// digest exactly as the executor does for a failed/transient outcome, so a
// replay of an already-recorded identical completion is idempotent.
func LostAttemptRestartOperation(attemptID, fence int64) (string, [32]byte) {
	seed := fmt.Sprintf("%d|%d|%t|%s", attemptID, fence, false, queue.FailureClassTransient)
	return fmt.Sprintf("op-%d-%d", attemptID, fence), sha256.Sum256([]byte(seed))
}

func isTransientCompleteError(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, ErrServiceUnavailable) || errors.Is(err, ErrTransportFailure)
}

// acknowledgesBeforeStarting reports whether a lost attempt in phase provably
// created no container, so it may be closed by Complete without the block.
func acknowledgesBeforeStarting(phase string) bool {
	switch phase {
	case state.AttemptPhaseClaimed, "fetching", "building":
		return true
	}
	return false
}

// recoverLostAttempts handles every leftover attempt record. It returns false
// when attempt state is corrupt: the agent then refuses to claim (fail closed,
// as for corrupt fence state). It never blocks the claim loop on an
// unreachable control plane.
func (a *Agent) recoverLostAttempts(ctx context.Context) bool {
	if a.Attempts == nil {
		return true
	}
	log := a.logger()
	records, err := a.Attempts.ListAttempts()
	if err != nil {
		log.Error("agent attempt state unreadable; refusing to claim")
		return false
	}
	for _, rec := range records {
		a.recoverLostAttempt(ctx, rec)
	}
	return true
}

func (a *Agent) recoverLostAttempt(ctx context.Context, rec state.AttemptRecord) {
	log := a.logger().With(logging.KeyAttemptID, rec.AttemptID, "phase", rec.Phase)
	del := func() {
		if err := a.Attempts.DeleteAttempt(rec.AttemptID); err != nil {
			log.Error("agent could not delete attempt record")
		}
	}
	if a.Activations != nil {
		_, owned, err := a.Activations.LoadActivation(rec.AttemptID)
		if err != nil {
			log.Error("lost attempt left untouched: activation record unreadable")
			return
		}
		if owned {
			log.Info("lost attempt left to activation recovery")
			del()
			return
		}
	}
	if !acknowledgesBeforeStarting(rec.Phase) {
		log.Info("lost attempt in " + rec.Phase + ": left to lease expiry")
		del()
		return
	}
	opID, digest := LostAttemptRestartOperation(rec.AttemptID, rec.Fence)
	var err error
	for try := 0; ; try++ {
		err = a.Client.Complete(ctx, rec.AttemptID, rec.Fence, queue.CompletionFailed, queue.FailureClassTransient, opID, digest)
		if err == nil || !isTransientCompleteError(ctx, err) || try >= len(lostAttemptRetryBackoff) {
			break
		}
		timer := time.NewTimer(lostAttemptRetryBackoff[try])
		select {
		case <-ctx.Done():
			timer.Stop()
			err = ctx.Err()
		case <-timer.C:
		}
		if ctx.Err() != nil {
			break
		}
	}
	switch {
	case err == nil:
		log.Info("lost attempt reported failed (agent restart)")
		del()
	case isTransientCompleteError(ctx, err):
		log.Warn("lost attempt not yet reported; record kept for the next restart")
	default:
		log.Info("lost attempt already closed by the control plane")
		del()
	}
}

// openAttempt writes the record for a freshly claimed lease. A failure means
// the attempt cannot be recovered after a crash, so the caller refuses to act.
func (a *Agent) openAttempt(lease Lease) error {
	if a.Attempts == nil {
		return nil
	}
	return a.Attempts.SaveAttempt(state.AttemptRecord{
		AttemptID: lease.AttemptID, JobID: lease.JobID, DeploymentID: lease.DeploymentID,
		EnvironmentID: lease.EnvironmentID, Fence: lease.Fence, Phase: state.AttemptPhaseClaimed,
	})
}

func (a *Agent) phaseRecorder(attemptID int64) PhaseRecorder {
	if a.Attempts == nil {
		return nil
	}
	return func(phase string) error { return a.Attempts.SetAttemptPhase(attemptID, phase) }
}

func (a *Agent) closeAttempt(attemptID int64) {
	if a.Attempts == nil {
		return
	}
	if err := a.Attempts.DeleteAttempt(attemptID); err != nil {
		a.logger().Error("agent could not delete attempt record", logging.KeyAttemptID, attemptID)
	}
}
