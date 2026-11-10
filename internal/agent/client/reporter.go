package agentclient

import (
	"context"
	"errors"
	"log"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"autodeploy/internal/agent/state"
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
	Logger *log.Logger

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

	kickOnce sync.Once
	kick     chan struct{}
}

// reportTrack is the in-memory retry state of one record. It is deliberately
// not persisted: after a restart every record is simply tried immediately.
type reportTrack struct {
	failures    int
	next        time.Time // zero: due now
	staleLogged bool
	lastTry     time.Time // zero: never tried; due records go oldest-try first
}

func (r *ActivationReporter) logger() *log.Logger {
	if r.Logger == nil {
		return log.Default()
	}
	return r.Logger
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
		r.logger().Printf("activation reporter: cannot scan pending activations; nothing sent or deleted, retrying: %v", err)
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
		r.logger().Printf("activation reporter: ERROR corrupt entry %q in pending activations; left untouched, operator action required", n)
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
	return ActivationFailed(p.FailureSummary)
}

// report makes one delivery attempt for p and updates tr. Log lines carry the
// attempt id, code and age only: never the failure summary.
// It reports whether the failure was outage-type (the caller stops the pass).
func (r *ActivationReporter) report(ctx context.Context, p state.PendingActivation, tr *reportTrack) (outage bool) {
	res, err := buildResult(p)
	if err != nil {
		r.logger().Printf("activation reporter: ERROR attempt %d: cannot build result from stored record; keeping record: %v", p.AttemptID, err)
		r.retryFixed(tr)
		r.warnStale(p, tr)
		return false
	}
	tr.lastTry = r.clock()
	cctx, cancel := context.WithTimeout(ctx, durOr(r.CallTimeout, defaultReportCallTimeout))
	err = r.Client.ReportActivationResult(cctx, p.AttemptID, p.Fence, p.OperationID, res)
	callTimedOut := cctx.Err() != nil && ctx.Err() == nil
	cancel()
	if ctx.Err() != nil {
		return false // shutting down; the record stays for the next start
	}

	terminal, fixed := false, false // fixed: retry at MaxBackoff, not exponential
	switch {
	case err == nil:
		terminal = true
	case errors.Is(err, ErrStaleLease):
		// Nothing was recorded server-side (attempt/fence mismatch, missing
		// operation row, or no longer the leased occupant): keep the evidence.
		fixed = true
		r.logger().Printf("activation reporter: ERROR attempt %d: server answered stale_lease with no confirmation the report was recorded; keeping record, retrying slowly", p.AttemptID)
	case errors.Is(err, ErrIntegrityConflict):
		code, _ := ConflictCode(err)
		if deleteOnConflict(code) {
			terminal = true
			r.logger().Printf("activation reporter: attempt %d: server answered conflict %q; report recorded or decided, terminal", p.AttemptID, code)
		} else {
			fixed = true
			if code == "" {
				code = "unknown"
			}
			r.logger().Printf("activation reporter: ERROR attempt %d: server answered conflict %q with no confirmation the report was recorded; keeping record, retrying slowly", p.AttemptID, code)
		}
	case errors.Is(err, ErrInvalidRequest):
		fixed = true
		r.logger().Printf("activation reporter: ERROR attempt %d: server rejected the report body; keeping record, retrying slowly", p.AttemptID)
	case errors.Is(err, ErrUnauthorized):
		fixed = true
		r.logger().Printf("activation reporter: attempt %d: agent credential rejected; keeping record, retrying slowly", p.AttemptID)
	default:
		// ErrServiceUnavailable, ErrTransportFailure, anything unknown.
		outage = callTimedOut || errors.Is(err, ErrServiceUnavailable) || errors.Is(err, ErrTransportFailure)
	}

	if terminal {
		if derr := r.Store.DeleteActivation(p.AttemptID); derr != nil {
			r.logger().Printf("activation reporter: attempt %d: report answered but record delete failed; will retry: %v", p.AttemptID, derr)
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

func (r *ActivationReporter) warnStale(p state.PendingActivation, tr *reportTrack) {
	if tr.staleLogged {
		return
	}
	age := r.clock().Sub(p.CreatedAt)
	if age >= durOr(r.StaleAfter, defaultReporterStaleAfter) {
		tr.staleLogged = true
		r.logger().Printf("activation reporter: WARNING attempt %d: result unreported for %s; traffic was switched and the control plane has not acknowledged it", p.AttemptID, age.Round(time.Second))
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
