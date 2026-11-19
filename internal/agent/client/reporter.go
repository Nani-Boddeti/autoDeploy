package agentclient

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"autodeploy/internal/agent/state"
	"autodeploy/internal/logging"
)

// Reporter defaults (tasks/10d-plan.md, 10d-4).
const (
	defaultReporterMinBackoff = 5 * time.Second
	defaultReporterMaxBackoff = 60 * time.Second
	defaultReporterStaleAfter = 15 * time.Minute
	defaultReportCallTimeout  = 30 * time.Second
)

// activationReportClient is the one control-plane call the reporter makes.
// *Client satisfies it.
type activationReportClient interface {
	ReportActivationResult(ctx context.Context, attemptID, fence int64, operationID string, r ActivationResult) error
}

// activationRecordStore is the slice of state.ActivationStore the reporter
// uses. state.ActivationStore satisfies it.
type activationRecordStore interface {
	ScanActivations() (records []state.PendingActivation, corrupt []string, err error)
	DeleteActivation(attemptID int64) error
	// DowngradeNotSwitched durably turns a record's not_switched outcome off
	// (idempotent). The reporter calls it when the control plane rejected the
	// outcome field, so the stripped (plain failure) shape survives a restart.
	DowngradeNotSwitched(attemptID int64) error
}

// drainArmer records the obligation to stop a predecessor release
// (tasks/10e-plan.md, 10e-3). state.DrainStore satisfies it.
type drainArmer interface {
	ArmDrain(d state.PendingDrain) error
}

// ActivationReporter delivers every result_ready pending activation to the
// control plane and keeps retrying until the server gives a TERMINAL answer,
// across outages and agent restarts (records reload from disk). A record is
// deleted only after a terminal answer: deleting earlier would lose a report
// after traffic switched.
//
// operation_mismatch is deliberately NOT terminal: the server rolls back and
// records nothing, so it must stay out of the delete list.
//
// Terminal means the server durably recorded the report or holds a final
// decision: 200, late_report_recorded, route_moved, integrity_conflict. Every
// other 409 (stale_lease, corrupt_state, constraint_violation, empty or unknown
// codes) can be returned after a rollback with nothing recorded, so deleting on
// it would destroy the only evidence that traffic may have switched; those keep
// the record and retry at MaxBackoff.
//
// It never calls Complete, never fails a job and never sets the local
// ReconciliationRequired flag (owner decision D-E): the server's slot block is
// the single authority. `authorized` records belong to the executor and are
// left untouched.
//
// A single goroutine (Run) processes due records sequentially, so at most one
// report per record is ever in flight. Run must not be called concurrently
// with itself.
type ActivationReporter struct {
	Client activationReportClient
	Store  activationRecordStore
	Logger *slog.Logger
	// Drains arms the predecessor drain (10e-3). On a 200 answer to a SUCCESS
	// report of a record with a predecessor the drain is saved BEFORE the
	// activation record is deleted; if arming fails the record is kept and the
	// report re-sent next pass (the server answers an identical replay 200
	// again, and arming an existing identical drain is a no-op that preserves
	// ArmedAt). No 409 -- even a delete-list one -- ever arms: those do not mean
	// the server acknowledged a verified success. A nil Drains never arms.
	Drains drainArmer

	// MinBackoff/MaxBackoff bound the per-record retry delay (defaults 5s/60s).
	MinBackoff, MaxBackoff time.Duration
	// StaleAfter is the age after which an unreported record is logged loudly,
	// once per record (default 15m).
	StaleAfter time.Duration
	// CallTimeout bounds one report call (default 30s).
	CallTimeout time.Duration

	// Test seams.
	now    func() time.Time
	after  func(d time.Duration) <-chan time.Time
	jitter func(max time.Duration) time.Duration // uniform in [0, max]
	// corruptLogEvery is how often a persisting corrupt entry is re-logged
	// (default 1h); corruptSeen is touched only by the Run goroutine.
	corruptLogEvery time.Duration
	corruptSeen     map[string]time.Time
	// armErrSeen rate-limits the "cannot arm" ERROR per attempt id (a stuck
	// drain file keeps the activation record forever); Run goroutine only.
	armErrSeen map[int64]time.Time

	kickOnce sync.Once
	kick     chan struct{}
}

// reportTrack is the in-memory retry state of one record. It is deliberately
// not persisted: after a restart every record is simply tried immediately.
type reportTrack struct {
	failures    int
	next        time.Time // zero: due now
	staleLogged bool
	// stripped: the control plane rejected the not_switched outcome during this
	// process's life; the record is reported as a plain failure even if the
	// durable downgrade could not be written. In memory only (the durable
	// downgrade is the restart-proof copy).
	stripped bool
	lastTry  time.Time // zero: never tried; due records go oldest-try first
}

// Validate checks the production requirements: a client, a record store and a
// drain store. Without Drains a predecessor would silently never be stopped, so
// cmd/agent must call this; a nil Drains stays allowed on a struct literal only
// for tests that exercise reporting alone.
func (r *ActivationReporter) Validate() error {
	if r.Client == nil || r.Store == nil || r.Drains == nil {
		return errors.New("activation reporter: client, activation store and drain store are required")
	}
	return nil
}

// logger is the reporter's component logger; a nil Logger discards.
func (r *ActivationReporter) logger() *slog.Logger {
	base := r.Logger
	if base == nil {
		base = slog.New(slog.DiscardHandler)
	}
	return base.With(logging.KeyComponent, "reporter")
}

func (r *ActivationReporter) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *ActivationReporter) wait(d time.Duration) (<-chan time.Time, func()) {
	if r.after != nil {
		return r.after(d), func() {}
	}
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

func durOr(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

func (r *ActivationReporter) minBackoff() time.Duration {
	return durOr(r.MinBackoff, defaultReporterMinBackoff)
}

func (r *ActivationReporter) maxBackoff() time.Duration {
	return max(durOr(r.MaxBackoff, defaultReporterMaxBackoff), r.minBackoff())
}

func (r *ActivationReporter) kickCh() chan struct{} {
	r.kickOnce.Do(func() { r.kick = make(chan struct{}, 1) })
	return r.kick
}

// Kick asks Run to report promptly (e.g. right after a new result_ready record
// was saved). It never blocks, may be called before Run starts, and kicks
// coalesce.
func (r *ActivationReporter) Kick() {
	select {
	case r.kickCh() <- struct{}{}:
	default:
	}
}

// Run reports pending activations until ctx is cancelled, then returns
// ctx.Err().
func (r *ActivationReporter) Run(ctx context.Context) error {
	kick := r.kickCh()
	tracks := map[int64]*reportTrack{}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay := r.pass(ctx, tracks)
		ch, stop := r.wait(delay)
		select {
		case <-ctx.Done():
			stop()
			return ctx.Err()
		case <-kick:
			stop()
		case <-ch:
		}
	}
}

// pass reports every due record once and returns how long to wait before the
// next pass. After an outage-type failure (transport, 503, call timeout) the
// pass stops: the remaining due records are deferred to the same time without
// counting a failure, since no call was made for them. Records are ordered by
// last try so a persistently failing record cannot starve the others.
func (r *ActivationReporter) pass(ctx context.Context, tracks map[int64]*reportTrack) time.Duration {
	recs, corruptNames, err := r.Store.ScanActivations()
	if err != nil {
		// Fail closed: never delete or skip on an unreadable directory.
		r.logger().Warn("activation reporter: cannot scan pending activations; nothing sent or deleted, retrying", logging.Err(err))
		return r.maxBackoff()
	}
	r.logCorrupt(corruptNames)
	present := make(map[int64]bool, len(recs))
	for _, p := range recs {
		present[p.AttemptID] = true
	}
	for id := range tracks {
		if !present[id] {
			delete(tracks, id)
		}
	}

	var ready []state.PendingActivation
	for _, p := range recs {
		if p.Phase != state.PhaseResultReady {
			continue
		}
		if tracks[p.AttemptID] == nil {
			tracks[p.AttemptID] = &reportTrack{}
		}
		ready = append(ready, p)
	}
	sort.SliceStable(ready, func(i, j int) bool {
		return tracks[ready[i].AttemptID].lastTry.Before(tracks[ready[j].AttemptID].lastTry)
	})

	var next time.Time
	consider := func(t time.Time) {
		if !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	var deferTo time.Time
	for _, p := range ready {
		tr := tracks[p.AttemptID]
		if ctx.Err() != nil {
			return 0
		}
		if !deferTo.IsZero() {
			if !r.clock().Before(tr.next) {
				// Deferred records inherit the failing record's next time
				// (jittered, <= MaxBackoff) without a failure charged: no
				// call was made for them.
				tr.next = deferTo
			}
			consider(tr.next)
			continue
		}
		if r.clock().Before(tr.next) {
			consider(tr.next)
			continue
		}
		if outage := r.report(ctx, p, tr); outage {
			deferTo = tr.next
		}
		consider(tr.next)
	}
	if ctx.Err() != nil {
		return 0
	}
	if next.IsZero() {
		return r.minBackoff() // idle: poll for records saved without a Kick
	}
	return max(next.Sub(r.clock()), 0)
}

// ReportAnswerIsTerminal reports whether an activation-result answer proves the
// server durably holds the report or a final decision for the operation (the
// only answers on which a pending record may be dropped). It is the single
// definition shared by the reporter and the executor's direct-report fallback.
func ReportAnswerIsTerminal(err error) bool {
	if err == nil {
		return true
	}
	code, ok := ConflictCode(err)
	return ok && deleteOnConflict(code)
}

// deleteOnConflict is an allow-list: only codes meaning the server durably
// recorded our report or holds a final decision for the operation. Every other
// 409 (including unknown or empty codes) keeps the record.
func deleteOnConflict(code string) bool {
	switch code {
	case ConflictLateReportRecorded, ConflictRouteMoved, ConflictIntegrity:
		return true
	}
	return false
}

// logCorrupt logs a corrupt entry the first time it is seen, then at most once
// per corruptLogEvery while it persists; names that disappear are forgotten.
func (r *ActivationReporter) logCorrupt(names []string) {
	if r.corruptSeen == nil {
		r.corruptSeen = map[string]time.Time{}
	}
	now := r.clock()
	current := make(map[string]bool, len(names))
	for _, n := range names {
		current[n] = true
		if last, ok := r.corruptSeen[n]; ok && now.Sub(last) < durOr(r.corruptLogEvery, time.Hour) {
			continue
		}
		r.corruptSeen[n] = now
		r.logger().Error("activation reporter: corrupt entry in pending activations; left untouched, operator action required", "entry", n)
	}
	for n := range r.corruptSeen {
		if !current[n] {
			delete(r.corruptSeen, n)
		}
	}
}

func buildResult(p state.PendingActivation) (ActivationResult, error) {
	if p.Succeeded {
		return ActivationSucceeded(p.ObservedRouteDigest), nil
	}
	if p.NotSwitched {
		return ActivationNotSwitched(p.FailureSummary)
	}
	return ActivationFailed(p.FailureSummary)
}

// isServerBadRequest reports a 400 invalid_request the control plane itself
// answered (not a local pre-send validation failure, which is also
// ErrInvalidRequest but says nothing about the server's version).
func isServerBadRequest(err error) bool {
	var bad errBadRequest
	return errors.As(err, &bad)
}

// report makes one delivery attempt for p and updates tr. Log lines carry the
// attempt id, code and age only: never the failure summary.
// It reports whether the failure was outage-type (the caller stops the pass).
func (r *ActivationReporter) report(ctx context.Context, p state.PendingActivation, tr *reportTrack) (outage bool) {
	res, err := buildResult(p)
	if err != nil {
		r.logger().Error("activation reporter: cannot build result from stored record; keeping record", logging.KeyAttemptID, p.AttemptID, logging.Err(err))
		r.retryFixed(tr)
		r.warnStale(p, tr)
		return false
	}
	tr.lastTry = r.clock()
	if tr.stripped {
		res = res.withoutOutcome()
	}
	send := func(res ActivationResult) (timedOut bool, err error) {
		cctx, cancel := context.WithTimeout(ctx, durOr(r.CallTimeout, defaultReportCallTimeout))
		err = r.Client.ReportActivationResult(cctx, p.AttemptID, p.Fence, p.OperationID, res)
		timedOut = cctx.Err() != nil && ctx.Err() == nil
		cancel()
		return timedOut, err
	}
	callTimedOut, err := send(res)
	if ctx.Err() != nil {
		return false // shutting down; the record stays for the next start
	}
	if res.NotSwitched() && isServerBadRequest(err) {
		// Server downgrade (an older control plane strict-decodes and rejects
		// the unknown outcome field). Strip it durably FIRST, so a restart
		// right after cannot send it again, then retry ONCE as a plain
		// failure. A 400 on that retry is handled below like any other 400:
		// there is no second strip and no loop. If the durable write fails the
		// in-memory flag still keeps this process from re-sending the outcome.
		r.logger().Warn("activation reporter: server rejected the not_switched outcome; reporting as a plain failure", logging.KeyAttemptID, p.AttemptID)
		tr.stripped = true
		if derr := r.Store.DowngradeNotSwitched(p.AttemptID); derr != nil {
			r.logger().Error("activation reporter: cannot persist the outcome downgrade; retrying as a plain failure in memory", logging.KeyAttemptID, p.AttemptID, logging.Err(derr))
		}
		callTimedOut, err = send(res.withoutOutcome())
		if ctx.Err() != nil {
			return false
		}
	}

	terminal, fixed := false, false // fixed: retry at MaxBackoff, not exponential
	switch {
	case err == nil:
		terminal = true
	case errors.Is(err, ErrStaleLease):
		// Nothing was recorded server-side (attempt/fence mismatch, missing
		// operation row, or no longer the leased occupant): keep the evidence.
		fixed = true
		r.logger().Error("activation reporter: server answered stale_lease with no confirmation the report was recorded; keeping record, retrying slowly", logging.KeyAttemptID, p.AttemptID)
	case errors.Is(err, ErrIntegrityConflict):
		code, _ := ConflictCode(err)
		if deleteOnConflict(code) {
			terminal = true
			r.logger().Info("activation reporter: server answered conflict; report recorded or decided, terminal", logging.KeyAttemptID, p.AttemptID, "conflict_code", code)
		} else {
			fixed = true
			if code == "" {
				code = "unknown"
			}
			r.logger().Error("activation reporter: server answered conflict with no confirmation the report was recorded; keeping record, retrying slowly", logging.KeyAttemptID, p.AttemptID, "conflict_code", code)
		}
	case errors.Is(err, ErrInvalidRequest):
		fixed = true
		r.logger().Error("activation reporter: server rejected the report body; keeping record, retrying slowly", logging.KeyAttemptID, p.AttemptID)
	case errors.Is(err, ErrUnauthorized):
		fixed = true
		r.logger().Warn("activation reporter: agent credential rejected; keeping record, retrying slowly", logging.KeyAttemptID, p.AttemptID)
	default:
		// ErrServiceUnavailable, ErrTransportFailure, anything unknown.
		outage = callTimedOut || errors.Is(err, ErrServiceUnavailable) || errors.Is(err, ErrTransportFailure)
	}

	if terminal && err == nil {
		if !r.armDrain(p) {
			// Keep the activation record: the obligation is not durable yet.
			r.retryBackoff(tr)
			return false
		}
	}
	if terminal {
		if derr := r.Store.DeleteActivation(p.AttemptID); derr != nil {
			r.logger().Warn("activation reporter: report answered but record delete failed; will retry", logging.KeyAttemptID, p.AttemptID, logging.Err(derr))
			r.retryBackoff(tr)
			return false
		}
		// Record gone; the entry is pruned on the next pass.
		tr.next = time.Time{}
		return false
	}

	if fixed {
		r.retryFixed(tr)
	} else {
		r.retryBackoff(tr)
	}
	r.warnStale(p, tr)
	return outage
}

// armDrain persists the drain obligation of an acknowledged (200) success
// with a predecessor. It reports whether the caller may delete the activation
// record: true when nothing is owed, the drain store is absent (logged once) or
// the drain is durable.
func (r *ActivationReporter) armDrain(p state.PendingActivation) bool {
	d, owed := state.DrainFromActivation(p, r.clock())
	if !owed {
		return true
	}
	if r.Drains == nil {
		r.logger().Error("activation reporter: a predecessor drain is owed but no drain store is configured; the predecessor will not be stopped", logging.KeyAttemptID, p.AttemptID)
		return true
	}
	if err := r.Drains.ArmDrain(d); err != nil {
		if r.armErrSeen == nil {
			r.armErrSeen = map[int64]time.Time{}
		}
		now := r.clock()
		if last, ok := r.armErrSeen[p.AttemptID]; !ok || now.Sub(last) >= durOr(r.corruptLogEvery, time.Hour) {
			r.armErrSeen[p.AttemptID] = now
			r.logger().Error("activation reporter: report acknowledged but the predecessor drain could not be armed; keeping the activation record, retrying (further occurrences logged hourly)", logging.KeyAttemptID, p.AttemptID, logging.Err(err))
		}
		return false
	}
	delete(r.armErrSeen, p.AttemptID)
	return true
}

func (r *ActivationReporter) warnStale(p state.PendingActivation, tr *reportTrack) {
	if tr.staleLogged {
		return
	}
	age := r.clock().Sub(p.CreatedAt)
	if age >= durOr(r.StaleAfter, defaultReporterStaleAfter) {
		tr.staleLogged = true
		r.logger().Warn("activation reporter: result unreported; traffic was switched and the control plane has not acknowledged it", logging.KeyAttemptID, p.AttemptID, "age", age.Round(time.Second).String())
	}
}

func (r *ActivationReporter) retryFixed(tr *reportTrack) {
	tr.failures++
	tr.next = r.clock().Add(r.maxBackoff())
}

// retryBackoff schedules the next try: exponential ceiling (MinBackoff doubled
// per failure, capped at MaxBackoff) with full jitter between MinBackoff and
// that ceiling.
func (r *ActivationReporter) retryBackoff(tr *reportTrack) {
	minB, maxB := r.minBackoff(), r.maxBackoff()
	ceil := minB
	for i := 0; i <= tr.failures && ceil < maxB; i++ {
		ceil *= 2
	}
	ceil = min(ceil, maxB)
	tr.failures++
	delay := minB
	if ceil > minB {
		j := r.jitter
		if j == nil {
			j = func(m time.Duration) time.Duration { return time.Duration(rand.Int64N(int64(m) + 1)) }
		}
		delay += min(max(j(ceil-minB), 0), ceil-minB)
	}
	tr.next = r.clock().Add(delay)
}
