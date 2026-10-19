package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Step 10c-2b step E: a report that reaches tx2 after the operation is already
// reconciliation_required (recorded by tx2 itself or by the sweeper's
// diversion) is recorded as EVIDENCE and never finalizes anything.
//
// APPEND, not replace. outcome_summary already holds the reason the
// environment is blocked (the first failure report, the digest mismatch, or
// the sweeper's expiry text); replacing it would destroy the original record
// of why the ambiguity exists. Each late report adds one entry:
//
//	<existing> | late report #<fp>: <body>
//
// where fp is 16 hex characters of a SHA-256 over the report's content. That
// fingerprint IS the replay ledger (the job event carries no payload): a
// replay is "an entry with this fingerprint is already in outcome_summary".
// The entry and its job event are written in one transaction, so the
// fingerprint exists iff its event was emitted, and a replay writes nothing.
//
// The column is capped at 2048 bytes (CHECK octet_length <= 2048). The entry
// is cut on a rune boundary to the room left; the marker (everything up to
// and including the colon) is never cut. When even the marker does not fit,
// nothing is written and no event is emitted (the evidence of that report is
// lost to the summary, bounded by the cap; an event without a ledger entry
// would duplicate on every replay, which is worse).
//
// SUCCESS RESERVE. A late success report is the decisive evidence (traffic may
// have switched), so the last lateSuccessReserve bytes of the column are usable
// by a success entry only; failure entries stop lateSuccessReserve short of the
// cap. The reserve holds TWO distinct longest success entries (the realistic
// case is a wrong-digest success followed by the matching one) and is
// guaranteed to exist because every writer of the block's original summary
// bounds it to maxActivationFailureSummaryBytes (512): even then a full-length
// late failure entry still fits beside the reserve (unit-tested from these
// constants). A third distinct success report may find no room: nothing is
// written and the answer is still 409 late_report_recorded (accepted).
const (
	maxOutcomeSummaryBytes = 2048
	lateReportSeparator    = " | "
	lateReportMarkerPrefix = "late report #"

	// lateSuccessReserve must hold two longest success entries plus their
	// separators (see TestLateSuccessEntryFitsTheReserve).
	lateSuccessReserve = 512
)

// activationReport is the content of one agent activation-result report, as
// tx2 and the fresh-transaction block path both need it to record evidence.
type activationReport struct {
	succeeded, digestMatches bool
	observedRouteDigest      [32]byte
	failureSummary           string // sanitized (failureOutcomeSummary)
}

// lateReportFingerprint identifies a report by content: kind plus the
// observed digest (success) or the sanitized failure summary (failure). The
// failure summary is already truncated to 512 bytes, so failures that differ
// only past byte 512 are treated as replays of each other.
func lateReportFingerprint(succeeded bool, observedRouteDigest [32]byte, failureSummary string) string {
	h := sha256.New()
	if succeeded {
		h.Write([]byte("success\x00"))
		h.Write(observedRouteDigest[:])
	} else {
		h.Write([]byte("failure\x00"))
		h.Write([]byte(failureSummary))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

func lateReportMarker(fingerprint string) string {
	return lateReportMarkerPrefix + fingerprint + ":"
}

// isOriginalBlockReplay reports whether a failure report is a replay of the
// one that CREATED the block: the summary equals the block's own text, which
// late-report entries may follow. Anchored on the entry marker so a summary
// that merely contains " | " does not match. Any failure report whose
// sanitized text equals the block's own text matches (accepted as idempotent).
func isOriginalBlockReplay(existing, failureSummary string) bool {
	return existing == failureSummary || strings.HasPrefix(existing, failureSummary+lateReportSeparator+lateReportMarkerPrefix)
}

type lateEvidenceOutcome int

const (
	lateEvidenceAppended lateEvidenceOutcome = iota
	lateEvidenceReplay
	lateEvidenceNoRoom
)

// lateSuccessBody is the success entry's text; its length is bounded by the
// format alone (fixed text, a bool, 64 hex digits). The flag precedes the
// digest so a cut can never drop it.
func lateSuccessBody(observedRouteDigest [32]byte, digestMatches bool) string {
	return fmt.Sprintf("agent reported success after the activation was blocked; matches authorized digest: %t; observed route digest sha256:%s", digestMatches, hex.EncodeToString(observedRouteDigest[:]))
}

func lateFailureBody(failureSummary string) string {
	return "agent reported failure after the activation was blocked: " + failureSummary
}

// appendLateEvidence returns the new outcome_summary and what happened. body
// is the human-readable evidence; it is sanitized here, and the whole result
// never exceeds maxOutcomeSummaryBytes or splits a rune.
func appendLateEvidence(existing, fingerprint, body string, success bool) (string, lateEvidenceOutcome) {
	marker := lateReportMarker(fingerprint)
	if strings.Contains(existing, marker) {
		return existing, lateEvidenceReplay
	}
	room := maxOutcomeSummaryBytes - len(existing) - len(lateReportSeparator)
	if !success {
		// The reserve belongs to success entries only.
		room -= lateSuccessReserve
	}
	if room < len(marker) {
		// Nothing is written and no event is emitted; the caller still answers
		// the same 409 late_report_recorded.
		return existing, lateEvidenceNoRoom
	}
	entry := marker
	if body = sanitizeBoundedText(body, maxOutcomeSummaryBytes); body != "" {
		entry += " " + body
	}
	return existing + lateReportSeparator + truncateOnRuneBoundary(entry, room), lateEvidenceAppended
}

type lateReportInput struct {
	jobID, attemptID, fence, operationID int64
	environmentID                        string
	attemptState                         string
	slotFence                            int64
	slotCurrentAttemptID                 *int64
	existingSummary                      string
	report                               activationReport
}

// leasedOccupant reports the attempt-shape rule: the attempt is still the
// leased occupant of the slot at this fence.
func leasedOccupant(attemptState string, slotFence, fence int64, slotCurrentAttemptID *int64, attemptID int64) bool {
	return attemptState == "leased" && slotFence == fence && slotCurrentAttemptID != nil && *slotCurrentAttemptID == attemptID
}

// writeLateEvidence is the non-committing core shared by tx2's late path and
// the fresh-transaction block path: append (or recognise a replay of) the
// report's entry, UPDATE the operation, emit the job event. appended=false
// means replay or no room: nothing was written. The caller holds the locks
// and owns commit/rollback.
func (r *AgentLeaseRepository) writeLateEvidence(ctx context.Context, tx pgx.Tx, jobID, attemptID, operationID int64, existingSummary string, rep activationReport) (appended bool, err error) {
	fingerprint := lateReportFingerprint(rep.succeeded, rep.observedRouteDigest, rep.failureSummary)
	body := lateFailureBody(rep.failureSummary)
	if rep.succeeded {
		body = lateSuccessBody(rep.observedRouteDigest, rep.digestMatches)
	}
	updated, outcome := appendLateEvidence(existingSummary, fingerprint, body, rep.succeeded)
	if outcome != lateEvidenceAppended {
		// Replay (already recorded) or no room: same answer, nothing written.
		return false, nil
	}
	tag, err := tx.Exec(ctx, `UPDATE release_operations SET outcome_summary=$2 WHERE id=$1 AND kind='activate' AND state='reconciliation_required'`, operationID, updated)
	if err != nil {
		return false, fmt.Errorf("record late activation report: %w", err)
	}
	if tag.RowsAffected() != 1 {
		// The operation row is locked by the caller, so this cannot happen.
		// Deliberately NOT ErrAgentLeaseStale: that is terminal for the agent,
		// and nothing durable exists; a plain error is retryable (503).
		return false, fmt.Errorf("record late activation report: operation %d not updated (rows=%d)", operationID, tag.RowsAffected())
	}
	if err = r.inject("late_report_operation"); err != nil {
		return false, err
	}
	if err = appendAgentJobEvent(ctx, tx, jobID, &attemptID, "activation_late_report"); err != nil {
		return false, err
	}
	if err = r.inject("late_report_event"); err != nil {
		return false, err
	}
	return true, nil
}

// recordLateReport handles a report against an operation already at
// reconciliation_required. The caller (reportActivationResult) holds, in ADR
// order: credential SHARE -> environment slot -> job -> attempt -> this
// operation row, all FOR UPDATE and read in that order. This adds, in order:
// the operation UPDATE (outcome_summary only; closed_at and state untouched,
// the one rr -> rr edge migration 000021 allows), then the job event INSERT.
// No deployment, route head or release lock is taken: nothing there is
// written, so the order cannot invert. It reads no clock (the UPDATE sets no
// timestamp), so there is nothing to disagree with another statement's clock.
//
// ATTEMPT-SHAPE RULE: evidence is accepted only while the reporting attempt is
// still the operation's live owner -- the leased occupant of the slot at this
// fence, the same predicate tx2's normal path applies (authority expiry is
// deliberately not consulted: the report may describe a syscall that outran
// the lease, and the sweeper's diversion leaves exactly this shape, leased).
// A closed attempt, a moved fence or another occupant is refused with
// ErrAgentLeaseStale and writes nothing: such a shape is not one production
// creates since step D, and an attempt that no longer owns the environment has
// no standing to annotate its blocked operation. Credential and attempt
// ownership were already verified by the caller.
//
// Not an occupantChecked error class: a failed write here is retryable (the
// evidence is not durable yet), never a terminal typed error.
func (r *AgentLeaseRepository) recordLateReport(ctx context.Context, tx pgx.Tx, in lateReportInput) error {
	// Unreachable in production: since step D the sweeper never closes an
	// attempt holding an unresolved activation (it diverts to the block and
	// leaves the attempt leased and the slot occupied), and only step G may
	// resolve it. Kept as a guard against a future writer breaking that.
	if !leasedOccupant(in.attemptState, in.slotFence, in.fence, in.slotCurrentAttemptID, in.attemptID) {
		return ErrAgentLeaseStale
	}
	appended, err := r.writeLateEvidence(ctx, tx, in.jobID, in.attemptID, in.operationID, in.existingSummary, in.report)
	if err != nil {
		return err
	}
	if !appended {
		return ErrAgentLeaseActivationLateReportRecorded
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit late activation report: %w", err)
	}
	return ErrAgentLeaseActivationLateReportRecorded
}

// traceDeclaredContradiction is the terminal arms' answer to a report that
// contradicts the recorded outcome. When the attempt has a 'reconciled' event
// the outcome was DECLARED (operator or agent) rather than finalized by tx2,
// so the report is real evidence the declaration may be wrong: ONE
// 'activation_late_report' event is appended for the attempt and committed
// (the operation row is closed and stays untouched). Replays add nothing: no
// event is appended while one with a higher sequence than the attempt's
// 'reconciled' event exists. The answer is ErrAgentLeaseIntegrityConflict
// either way; only a failed write or commit is returned as a plain, retryable
// error (the caller has not classified it: occupantChecked is still false),
// and the retry is deduped. The caller holds the job lock (event sequence) and
// has verified the attempt's server. With no 'reconciled' event the behaviour
// is exactly the plain conflict answer.
func (r *AgentLeaseRepository) traceDeclaredContradiction(ctx context.Context, tx pgx.Tx, jobID, attemptID int64) error {
	var declared, traced bool
	if err := tx.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM agent_job_events WHERE job_id=$1 AND attempt_id=$2 AND event_type='reconciled'),
		EXISTS(SELECT 1 FROM agent_job_events t WHERE t.job_id=$1 AND t.attempt_id=$2 AND t.event_type='activation_late_report'
			AND t.sequence > (SELECT max(sequence) FROM agent_job_events WHERE job_id=$1 AND attempt_id=$2 AND event_type='reconciled'))`,
		jobID, attemptID).Scan(&declared, &traced); err != nil {
		return fmt.Errorf("look up declared reconciliation events: %w", err)
	}
	if !declared || traced {
		return ErrAgentLeaseIntegrityConflict
	}
	if err := appendAgentJobEvent(ctx, tx, jobID, &attemptID, "activation_late_report"); err != nil {
		return err
	}
	if err := r.inject("declared_late_report_event"); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit declared-outcome late report: %w", err)
	}
	return ErrAgentLeaseIntegrityConflict
}
