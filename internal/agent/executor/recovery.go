package executor

import (
	"log"

	"autodeploy/internal/agent/state"
	"autodeploy/internal/runtime/traefik"
)

// RecoveryStore is the slice of state.ActivationStore recovery uses.
// state.ActivationStore satisfies it.
type RecoveryStore interface {
	ScanActivations() (records []state.PendingActivation, corrupt []string, err error)
	SaveActivation(p state.PendingActivation) error
}

// RecoverActivations converts every `authorized` pending activation into a
// result_ready FAILURE so the reporter can deliver it. It must run only
// while no executor is running: at startup before the first claim, and after
// each lease returns (the agent runs leases one at a time and waits for the
// executor on every exit path). Only then is it guaranteed that no executor
// owns an `authorized` record. A stranded `authorized` record means a crash (or a
// failed result save plus a failed direct report) happened after authorization,
// so the route may or may not have been switched and no result was recorded.
//
// The failure carries the on-disk and authorized route digests as evidence. It
// is reported as a FAILURE even when the two digests are equal: the agent never
// declares an outcome itself (owner decision D-B) -- the server blocks and an
// operator declares. Nothing is rolled back.
//
// result_ready records are untouched (the reporter owns them), as are corrupt
// ones (the reporter logs them). A directory-level scan error is logged loudly
// and recovery stops without failing startup; the reporter keeps failing closed
// and logging. A save error on one record is logged and the rest still run.
// It returns the number of records converted.
func RecoverActivations(store RecoveryStore, routeDir string, logger *log.Logger) int {
	return recoverActivations(store, routeDir, traefik.ReadDigest, logger)
}

func recoverActivations(store RecoveryStore, routeDir string, readDigest func(dir, environmentID string) (string, bool, error), logger *log.Logger) int {
	if logger == nil {
		logger = log.Default()
	}
	records, _, err := store.ScanActivations()
	if err != nil {
		logger.Printf("agent activation recovery ERROR: cannot scan pending activations; stranded authorized records are NOT recovered this pass: %v", err)
		return 0
	}
	converted := 0
	for _, rec := range records {
		if rec.Phase != state.PhaseAuthorized {
			continue
		}
		onDisk := "unreadable"
		if d, exists, rerr := readDigest(routeDir, rec.EnvironmentID); rerr == nil {
			if exists {
				onDisk = d
			} else {
				onDisk = "absent"
			}
		}
		rec.Phase = state.PhaseResultReady
		rec.Succeeded = false
		rec.FailureSummary = buildFailureSummary("recovery", "activation result was neither saved nor reported (agent restart or failed result save)", onDisk, routeDigestString(rec.RouteDigest))
		if serr := store.SaveActivation(rec); serr != nil {
			logger.Printf("agent activation recovery ERROR attempt %d: cannot convert the authorized record, left untouched (operator action may be required): %v", rec.AttemptID, serr)
			continue
		}
		converted++
		logger.Printf("agent activation recovery attempt %d: authorized record converted to a failure report (on-disk route digest: %s)", rec.AttemptID, onDisk)
	}
	return converted
}
