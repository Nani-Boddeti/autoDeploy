package executor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/agent/state"
	"autodeploy/internal/runtime/docker"
	"autodeploy/internal/runtime/traefik"
)

// The predecessor drainer (tasks/10e-plan.md, 10e-4). After the control plane
// acknowledged a successful activation, the agent owes a STOP of the previous
// release once drain_seconds have elapsed. The obligation is a durable
// state.PendingDrain record; the Drainer is the single goroutine that works
// through them, across restarts.
//
// It stops, never removes or kills: DrainDocker deliberately has no such
// method, and the engine client has none either. Every guard fails CLOSED --
// on any doubt the predecessor keeps running and the record stays as evidence.
//
// No tombstone is written after a completed drain (record deleted). A late
// re-arm -- a crash between ArmDrain and the activation-record delete, so the
// reporter replays and re-arms after the drain already completed -- yields a
// second pass over an already-stopped container (Engine answers 304, treated
// as success) and the route-digest guard still applies, so it is harmless. A
// tombstone would add durable state to protect against nothing.
//
// Stop failures were agent-local evidence only in 10e (owner decision D-c).
// Since step 20B every terminal outcome (stopped, already_stopped, gone,
// stop_failed, route_moved, identity_error) is also REPORTED to the control
// plane (visibility only: a failure never undoes or blocks anything). The
// terminal outcome is first made durable as phase report_pending, then sent;
// the record is only deleted (success outcomes) or marked Reported
// (failure outcomes, kept as local evidence) after a 200 or a terminal 409.
// Transient errors keep report_pending and are retried with the drain backoff,
// so an unacknowledged outcome is never lost across crashes or outages.

const (
	defaultDrainStopGrace = 30 * time.Second
	// maxDrainStopGrace mirrors the engine client's 300s stop-grace bound.
	maxDrainStopGrace         = 300 * time.Second
	defaultDrainWaitMargin    = 30 * time.Second
	defaultDrainMaxAttempts   = 5
	defaultDrainRetryBackoff  = 30 * time.Second
	defaultDrainMaxBackoff    = 10 * time.Minute
	defaultDrainPollInterval  = 30 * time.Second
	defaultDrainWaitInterval  = time.Second
	defaultDrainLogEvery      = time.Hour
	drainSummaryRouteChanged  = "route changed since the switch; predecessor not stopped"
	drainSummaryIdentityError = "identity mismatch; not stopped"
)

// DrainStore is the slice of state.DrainStore the drainer uses.
// state.DrainStore satisfies it.
type DrainStore interface {
	ScanDrains() (records []state.PendingDrain, corruptNames []string, err error)
	SaveDrain(d state.PendingDrain) error
	DeleteDrain(attemptID int64) error
}

// DrainDocker is the Engine capability the drainer needs: inspect, label
// lookup, stop and wait. There is intentionally NO remove or kill method.
// *docker.Client satisfies it.
type DrainDocker interface {
	InspectContainer(ctx context.Context, id string) (docker.ContainerInspect, error)
	ListContainersByLabels(ctx context.Context, labels map[string]string) ([]docker.ContainerSummary, error)
	StopContainer(ctx context.Context, id string, grace time.Duration) error
	WaitStopped(ctx context.Context, id string, timeout, interval time.Duration) (docker.ContainerInspect, error)
}

// DrainReporter sends a terminal drain outcome to the control plane.
// *agentclient.Client satisfies it.
type DrainReporter interface {
	ReportDrainResult(ctx context.Context, attemptID int64, predecessorReleaseID, predecessorRuntimeInstanceID, outcome, summary string) error
}

// Drainer stops predecessor releases whose drain window elapsed. Run must not
// be called concurrently with itself.
type Drainer struct {
	Store  DrainStore
	Docker DrainDocker
	// RouteDir is the Traefik file-provider directory (route-digest guard).
	RouteDir string
	// Reporter delivers terminal outcomes (required).
	Reporter DrainReporter
	Logger   *log.Logger

	// StopGrace is the engine stop grace: whole seconds in 1..300; zero uses 30s.
	StopGrace time.Duration
	// WaitTimeout bounds WaitStopped; zero uses StopGrace + 30s.
	WaitTimeout time.Duration
	// MaxAttempts is the number of failed stop attempts after which a retryable
	// failure becomes permanent; zero uses 5.
	MaxAttempts int
	// RetryBackoff is the delay after the first failed attempt (doubling per
	// attempt, capped at MaxRetryBackoff); zero uses 30s and 10m.
	RetryBackoff, MaxRetryBackoff time.Duration
	// PollInterval caps how long Run sleeps between passes; zero uses 30s.
	PollInterval time.Duration

	// Seams; nil uses the real implementation.
	ReadDigest func(dir, environmentID string) (string, bool, error)
	Now        func() time.Time
	// Monotonic is the clock for the first-seen bound (default time.Now, whose
	// monotonic reading ignores wall-clock steps).
	Monotonic func() time.Time
	sleep     func(ctx context.Context, d time.Duration) error

	logged      map[string]time.Time // rate-limit state, Run goroutine only
	reportNext  map[int64]time.Time  // earliest next report try (monotonic), by attempt
	reportFails map[int64]int
	seen        map[drainKey]time.Time
}

// drainKey identifies one waiting state of a record: the first time the
// drainer sees it bounds the wait by the nominal delay even if the wall clock
// steps backwards.
type drainKey struct {
	attempt  int64
	phase    state.DrainPhase
	attempts int
}

func (d *Drainer) mono() time.Time {
	if d.Monotonic != nil {
		return d.Monotonic()
	}
	return time.Now()
}

func (d *Drainer) logger() *log.Logger {
	if d.Logger == nil {
		return log.Default()
	}
	return d.Logger
}

func (d *Drainer) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Drainer) readDigest(dir, env string) (string, bool, error) {
	if d.ReadDigest != nil {
		return d.ReadDigest(dir, env)
	}
	return traefik.ReadDigest(dir, env)
}

func (d *Drainer) stopGrace() time.Duration { return durOrDefault(d.StopGrace, defaultDrainStopGrace) }
func (d *Drainer) waitTimeout() time.Duration {
	return durOrDefault(d.WaitTimeout, d.stopGrace()+defaultDrainWaitMargin)
}
func (d *Drainer) maxAttempts() int {
	if d.MaxAttempts > 0 {
		return d.MaxAttempts
	}
	return defaultDrainMaxAttempts
}
func (d *Drainer) pollInterval() time.Duration {
	return durOrDefault(d.PollInterval, defaultDrainPollInterval)
}

func durOrDefault(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// backoff is the delay before retry number attempts+1.
func (d *Drainer) backoff(attempts int) time.Duration {
	base := durOrDefault(d.RetryBackoff, defaultDrainRetryBackoff)
	limit := durOrDefault(d.MaxRetryBackoff, defaultDrainMaxBackoff)
	delay := base
	for i := 1; i < attempts && delay < limit; i++ {
		delay *= 2
	}
	if delay > limit {
		delay = limit
	}
	return delay
}

// Validate checks the configuration; Run repeats it, and cmd/agent calls it at
// startup so a misconfigured drainer fails startup instead of being skipped.
func (d *Drainer) Validate() error {
	if d.Store == nil || d.Docker == nil || d.RouteDir == "" || d.Reporter == nil {
		return errors.New("agent drainer: store, docker client, route dir and reporter are required")
	}
	if g := d.stopGrace(); g < time.Second || g > maxDrainStopGrace || g%time.Second != 0 {
		return errors.New("agent drainer: stop grace must be whole seconds within 1..300")
	}
	return nil
}

// Run processes drain records until ctx is cancelled, then returns ctx.Err().
// It returns an error immediately only for an invalid configuration.
func (d *Drainer) Run(ctx context.Context) error {
	if err := d.Validate(); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		wait := d.pass(ctx)
		if err := d.doSleep(ctx, wait); err != nil {
			return err
		}
	}
}

func (d *Drainer) doSleep(ctx context.Context, wait time.Duration) error {
	if d.sleep != nil {
		return d.sleep(ctx, wait)
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// logLimited logs at most once per hour per key.
func (d *Drainer) logLimited(key, format string, args ...any) {
	if d.logged == nil {
		d.logged = map[string]time.Time{}
	}
	now := d.now()
	if last, ok := d.logged[key]; ok && now.Sub(last) < defaultDrainLogEvery && !now.Before(last) {
		return
	}
	d.logged[key] = now
	d.logger().Printf(format, args...)
}

// pass scans once, processes every due record and returns how long to sleep
// (never more than the poll interval).
func (d *Drainer) pass(ctx context.Context) time.Duration {
	poll := d.pollInterval()
	records, corruptNames, err := d.Store.ScanDrains()
	if err != nil {
		d.logLimited("scan", "agent drainer ERROR: cannot scan pending drains; predecessors are NOT stopped until this clears: %v", err)
		return poll
	}
	for _, name := range corruptNames {
		d.logLimited("corrupt:"+name, "agent drainer ERROR: pending drain entry %q is not a valid record; left untouched (operator action required)", name)
	}
	wait := poll
	d.pruneSeen(records)
	for _, rec := range records {
		if ctx.Err() != nil {
			return poll
		}
		remaining, due, tracked := d.dueIn(rec)
		switch {
		case !tracked:
			d.logLimited(fmt.Sprintf("failed:%d", rec.AttemptID),
				"agent drainer ERROR attempt %d: predecessor release %s (runtime %s) was NOT stopped and will not be retried automatically: %s",
				rec.AttemptID, rec.PredecessorReleaseID, rec.PredecessorRuntimeInstanceID, rec.FailureSummary)
		case due:
			d.process(ctx, rec)
		case remaining < wait:
			wait = remaining
		}
	}
	return wait
}

func (d *Drainer) pruneSeen(records []state.PendingDrain) {
	live := make(map[drainKey]bool, len(records))
	for _, r := range records {
		live[drainKey{r.AttemptID, r.Phase, r.Attempts}] = true
	}
	for k := range d.seen {
		if !live[k] {
			delete(d.seen, k)
		}
	}
}

// bound caps remaining (from the wall clock) by what is left of the nominal
// delay measured on the monotonic clock since the state was first seen, so a
// wall clock stepped backwards by D delays the stop by at most the delay, not
// delay + D.
func (d *Drainer) bound(rec state.PendingDrain, remaining, delay time.Duration) time.Duration {
	if d.seen == nil {
		d.seen = map[drainKey]time.Time{}
	}
	k := drainKey{rec.AttemptID, rec.Phase, rec.Attempts}
	first, ok := d.seen[k]
	if !ok {
		first = d.mono()
		d.seen[k] = first
	}
	if left := delay - d.mono().Sub(first); left < remaining {
		remaining = left
	}
	return remaining
}

// dueIn reports how long until rec is due. tracked is false for a record the
// drainer will not act on again (permanent stop_failed). A wall clock that
// jumped backwards never makes the wait exceed the nominal delay.
func (d *Drainer) dueIn(rec state.PendingDrain) (remaining time.Duration, due, tracked bool) {
	now := d.now()
	switch rec.Phase {
	case state.DrainPending:
		window := time.Duration(rec.DrainSeconds) * time.Second
		remaining = rec.ArmedAt.Add(window).Sub(now)
		if remaining > window {
			remaining = window
		}
		remaining = d.bound(rec, remaining, window)
	case state.DrainStopFailed:
		if !rec.Retryable || rec.Attempts >= d.maxAttempts() {
			return 0, false, false
		}
		delay := d.backoff(rec.Attempts)
		remaining = rec.UpdatedAt.Add(delay).Sub(now)
		if remaining > delay {
			remaining = delay
		}
		remaining = d.bound(rec, remaining, delay)
	case state.DrainReportPending:
		if next, ok := d.reportNext[rec.AttemptID]; ok {
			remaining = next.Sub(d.mono())
		}
	default:
		return 0, false, false
	}
	return remaining, remaining <= 0, true
}

// process runs the guards and the stop for one due record.
func (d *Drainer) process(ctx context.Context, rec state.PendingDrain) {
	if rec.Phase == state.DrainReportPending {
		d.report(ctx, rec)
		return
	}
	// 1. Identity sanity (the store validates this too; re-checked so a
	// corrupted-but-valid record can never name the new release).
	if rec.PredecessorRuntimeInstanceID == rec.BackendRuntimeInstanceID || rec.PredecessorReleaseID == rec.ReleaseID {
		d.fail(ctx, rec, state.DrainOutcomeIdentityError, drainSummaryIdentityError+": stop target equals the new release or runtime instance")
		return
	}

	// 2. Route guard: the route must still be exactly what the switch left.
	onDisk, exists, err := d.readDigest(d.RouteDir, rec.EnvironmentID)
	observed, parsed := parseRouteDigestString(onDisk)
	if err != nil || !exists || !parsed || observed != rec.ObservedRouteDigest {
		d.fail(ctx, rec, state.DrainOutcomeRouteMoved, drainSummaryRouteChanged)
		return
	}

	// 3-6. Resolve by container name, exact label identity (owner and server
	// labels are not in the drain record and deliberately not checked: the
	// runtime instance id is the decimal attempt id, globally unique, so
	// environment + release + runtime-instance + managed + kind identify
	// exactly one container), stop by inspected ID, wait. Shared with the
	// executor's failed-candidate stop (stopguard.go).
	res := guardedStop(ctx, d.Docker, stopTarget{
		EnvironmentID:           rec.EnvironmentID,
		ReleaseID:               rec.PredecessorReleaseID,
		RuntimeInstanceID:       rec.PredecessorRuntimeInstanceID,
		ForbidReleaseID:         rec.ReleaseID,
		ForbidRuntimeInstanceID: rec.BackendRuntimeInstanceID,
	}, d.stopGrace(), d.waitTimeout(), defaultDrainWaitInterval)
	name, _ := docker.ContainerName(rec.PredecessorRuntimeInstanceID)
	switch res.Status {
	case stopInterrupted:
	case stopInvalid:
		d.fail(ctx, rec, state.DrainOutcomeIdentityError, drainSummaryIdentityError+": invalid runtime instance id")
	case stopMismatch:
		d.fail(ctx, rec, state.DrainOutcomeIdentityError, drainSummaryIdentityError+": "+res.Reason)
	case stopAlreadyStopped:
		d.done(ctx, rec, state.DrainOutcomeAlreadyStopped, "predecessor already stopped")
	case stopStopped:
		d.done(ctx, rec, state.DrainOutcomeStopped, "predecessor stopped")
	case stopGone:
		switch res.Step {
		case stepInspect:
			// Any 404 discharges the obligation; WARN so an unexpected 404
			// (proxy, odd daemon path) is visible rather than a quiet INFO.
			d.logger().Printf("agent drainer WARN attempt %d: inspect answered not-found for predecessor %s", rec.AttemptID, name)
			d.done(ctx, rec, state.DrainOutcomeGone, "predecessor container is already gone; nothing to stop")
		case stepStop:
			d.done(ctx, rec, state.DrainOutcomeGone, "predecessor container vanished before the stop")
		default:
			d.done(ctx, rec, state.DrainOutcomeGone, "predecessor container vanished while stopping")
		}
	case stopEngineError:
		op := map[stopStep]string{
			stepInspect: "inspect predecessor",
			stepLabels:  "look up predecessor labels",
			stepStop:    "stop predecessor",
			stepWait:    "wait for predecessor to stop",
		}[res.Step]
		d.failEngine(ctx, rec, op, res.Err)
	}
}

func alreadyStopped(status string) bool {
	switch status {
	case "exited", "dead", "created":
		return true
	}
	return false
}

// done ends the drain successfully: the outcome is made durable as
// report_pending and reported; the record is deleted once acknowledged.
func (d *Drainer) done(ctx context.Context, rec state.PendingDrain, outcome, why string) {
	d.logger().Printf("agent drainer INFO attempt %d: %s (release %s, runtime %s)", rec.AttemptID, why, rec.PredecessorReleaseID, rec.PredecessorRuntimeInstanceID)
	d.terminal(ctx, rec, outcome, why)
}

// failEngine classifies an engine error: transient ones are retried (bounded),
// configuration/identifier ones are permanent.
func (d *Drainer) failEngine(ctx context.Context, rec state.PendingDrain, op string, err error) {
	permanent := errors.Is(err, docker.ErrInvalidIdentifier)
	var ee *docker.EngineError
	if errors.As(err, &ee) && ee.Class == docker.ErrClassConfiguration {
		permanent = true
	}
	summary := fmt.Sprintf("%s failed: %s", op, truncateUTF8(err.Error(), maxReasonBytes))
	if permanent {
		d.fail(ctx, rec, state.DrainOutcomeStopFailed, summary)
		return
	}
	attempts := rec.Attempts + 1
	rec.Attempts = attempts
	if attempts < d.maxAttempts() {
		d.save(rec, summary, true)
		return
	}
	summary = fmt.Sprintf("%s (gave up after %d attempts)", summary, attempts)
	d.logger().Printf("agent drainer ERROR attempt %d: predecessor release %s is still running; retries exhausted: %s", rec.AttemptID, rec.PredecessorReleaseID, summary)
	d.fail(ctx, rec, state.DrainOutcomeStopFailed, summary)
}

// fail ends the drain with a permanent failure outcome (no retry), reported to
// the control plane and kept locally as evidence.
func (d *Drainer) fail(ctx context.Context, rec state.PendingDrain, outcome, summary string) {
	d.logger().Printf("agent drainer ERROR attempt %d: predecessor release %s (runtime %s) NOT stopped: %s", rec.AttemptID, rec.PredecessorReleaseID, rec.PredecessorRuntimeInstanceID, summary)
	d.terminal(ctx, rec, outcome, summary)
}

// resultSummary bounds and sanitizes text for the report: valid UTF-8, no
// control characters (the control plane's column rejects them), never empty.
func resultSummary(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, truncateUTF8(s, maxFailureSummaryBytes))
	if strings.TrimSpace(s) == "" {
		return "no detail"
	}
	return s
}

// terminal makes the outcome durable (report_pending) and then reports it in
// the same pass. If the save fails nothing changed on disk and the whole step
// is re-run on a later pass.
func (d *Drainer) terminal(ctx context.Context, rec state.PendingDrain, outcome, summary string) {
	rec.Phase = state.DrainReportPending
	rec.Outcome = outcome
	rec.ResultSummary = resultSummary(summary)
	rec.FailureSummary = ""
	rec.Retryable = false
	if err := d.Store.SaveDrain(rec); err != nil {
		d.logger().Printf("agent drainer ERROR attempt %d: cannot persist the drain outcome %s (will retry the whole step): %v", rec.AttemptID, outcome, err)
		return
	}
	d.report(ctx, rec)
}

// report sends a report_pending record. 200 and a terminal 409 acknowledge it;
// any other error keeps the record and schedules a retry with the drain backoff.
func (d *Drainer) report(ctx context.Context, rec state.PendingDrain) {
	err := d.Reporter.ReportDrainResult(ctx, rec.AttemptID, rec.PredecessorReleaseID, rec.PredecessorRuntimeInstanceID, rec.Outcome, rec.ResultSummary)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		if code, isConflict := agentclient.ConflictCode(err); isConflict {
			d.logger().Printf("agent drainer WARN attempt %d: control plane refused the drain result with a terminal conflict %q; not retrying", rec.AttemptID, code)
		} else {
			if d.reportFails == nil {
				d.reportFails, d.reportNext = map[int64]int{}, map[int64]time.Time{}
			}
			d.reportFails[rec.AttemptID]++
			d.reportNext[rec.AttemptID] = d.mono().Add(d.backoff(d.reportFails[rec.AttemptID]))
			d.logLimited(fmt.Sprintf("report:%d", rec.AttemptID), "agent drainer ERROR attempt %d: drain result %s not delivered yet (kept, will retry): %v", rec.AttemptID, rec.Outcome, err)
			return
		}
	}
	delete(d.reportFails, rec.AttemptID)
	delete(d.reportNext, rec.AttemptID)
	d.acknowledged(rec)
}

// acknowledged finishes a delivered record: success outcomes are deleted,
// failure outcomes stay as local evidence marked Reported.
func (d *Drainer) acknowledged(rec state.PendingDrain) {
	if !state.DrainOutcomeIsFailure(rec.Outcome) {
		if err := d.Store.DeleteDrain(rec.AttemptID); err != nil {
			d.logger().Printf("agent drainer ERROR attempt %d: drain result delivered but the record could not be deleted (will re-report, idempotent): %v", rec.AttemptID, err)
			return
		}
		// Forget first-seen times for this attempt so a re-arm (no-tombstone
		// path) waits its own full drain window.
		for k := range d.seen {
			if k.attempt == rec.AttemptID {
				delete(d.seen, k)
			}
		}
		return
	}
	rec.Phase = state.DrainStopFailed
	rec.FailureSummary = rec.ResultSummary
	rec.ResultSummary = ""
	rec.Reported = true
	if err := d.Store.SaveDrain(rec); err != nil {
		d.logger().Printf("agent drainer ERROR attempt %d: drain result delivered but the evidence record could not be updated (will re-report, idempotent): %v", rec.AttemptID, err)
	}
}

func (d *Drainer) save(rec state.PendingDrain, summary string, retryable bool) {
	rec.Phase = state.DrainStopFailed
	rec.FailureSummary = boundSummary(summary)
	rec.Retryable = retryable
	if err := d.Store.SaveDrain(rec); err != nil {
		d.logger().Printf("agent drainer ERROR attempt %d: cannot persist the stop failure (will retry the whole step): %v", rec.AttemptID, err)
	}
}
