package executor

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/agent/logship"
	"autodeploy/internal/agent/state"
	"autodeploy/internal/queue"
	"autodeploy/internal/runtime/traefik"
)

// The activation sequence (Step 10d-5a). After a passing health gate:
//
//  1. Activate (control plane authorizes the switch).
//  2. Persist a PendingActivation{authorized} BEFORE touching the route.
//  3. Re-render the route, guard its revision, write, verify, probe -- under a
//     context detached from lease cancellation, ending ALWAYS in a
//     result_ready record (success or failure).
//  4. Kick the reporter; return an Outcome that makes runLease send neither
//     Complete nor CancelAck.
//
// An `authorized` record left behind (a crash, or the result save and the
// direct report both failing) is converted into a result_ready failure with
// on-disk evidence by RecoverActivations (recovery.go) at agent startup. The reporter intentionally
// ignores `authorized` records because the executor may still own them.
//
// After authorization nothing is ever rolled back or rewritten (D4 / ADR 0005:
// blind resolution is forbidden); a failure is reported as evidence and the
// operator resolves it.

const (
	// maxFailureSummaryBytes mirrors the server's 1-2048 byte bound
	// (agentclient's maxActivationFailureSummaryLen is unexported).
	maxFailureSummaryBytes = 2048
	// maxReasonBytes bounds the free-text reason so the trailing digest
	// evidence always survives truncation.
	maxReasonBytes = 1024

	imageDigestPrefix = "sha256:"

	defaultActivateAttempts = 4
	defaultActivateBackoff  = 500 * time.Millisecond
	defaultVerifyTimeout    = 30 * time.Second
	defaultVerifyInterval   = time.Second
	// activationSlack is added to the probe and verify budgets to form the
	// default overall deadline of the detached post-authorization work.
	activationSlack  = 2 * time.Minute
	minTimeoutMargin = 30 * time.Second

	defaultSaveAttempts   = 3
	defaultSaveBackoff    = 100 * time.Millisecond
	defaultReportAttempts = 5
	defaultReportBackoff  = 500 * time.Millisecond
	maxReportBackoff      = 5 * time.Second
	// directReportTimeout bounds the best-effort direct failure report.
	directReportTimeout = 30 * time.Second
)

// ActivationClient is the control-plane capability the activation sequence
// needs. *agentclient.Client satisfies it.
type ActivationClient interface {
	Activate(ctx context.Context, attemptID, fence int64, imageDigest [32]byte) (agentclient.ActivateResponse, error)
	ReportActivationResult(ctx context.Context, attemptID, fence int64, operationID string, result agentclient.ActivationResult) error
}

// RouteObserver checks Traefik's API for the written route.
// *traefik.Observer satisfies it.
type RouteObserver interface {
	Verify(ctx context.Context, r traefik.EnvironmentRoute) (traefik.VerifyResult, error)
}

// ActivationStore persists pending activation records.
// state.ActivationStore satisfies it.
type ActivationStore interface {
	SaveActivation(p state.PendingActivation) error
	DeleteActivation(attemptID int64) error
}

// RouteRevisionStore is the local route-revision guard (D-C).
// state.RouteRevisionStore satisfies it.
type RouteRevisionStore interface {
	LoadRouteRevision(environmentID string) (int64, error)
	RecordRouteRevision(environmentID string, rev int64) error
}

// DrainArmer records the obligation to stop the predecessor release after a
// server-acknowledged success (10e-3). state.DrainStore satisfies it. REQUIRED
// in ActivationConfig (newActivationRunner rejects nil): without it a
// predecessor would silently never be stopped.
type DrainArmer interface {
	ArmDrain(d state.PendingDrain) error
}

// Kicker wakes the activation reporter. *agentclient.ActivationReporter
// satisfies it. A nil Kicker is allowed.
type Kicker interface{ Kick() }

// ActivationConfig configures the post-health-gate activation sequence.
type ActivationConfig struct {
	Client   ActivationClient
	Observer RouteObserver
	Store    ActivationStore
	// Revisions is the local route-revision guard store.
	Revisions RouteRevisionStore
	// Kicker is optional.
	Kicker Kicker
	// Drains is required; see DrainArmer.
	Drains DrainArmer

	// RouteDir is the Traefik file-provider directory.
	RouteDir string
	// ProbeAddress, ProbePath and ProbeTimeout configure
	// traefik.ProbeDataPlane (loopback listener, request path, overall
	// deadline -- generous enough for a first-deploy ACME issuance).
	ProbeAddress string
	ProbePath    string
	ProbeTimeout time.Duration

	// VerifyTimeout bounds polling of Observer.Verify while Traefik has not
	// yet loaded the route; zero uses 30s.
	VerifyTimeout time.Duration
	// Timeout is the overall deadline of the detached post-authorization
	// work; zero uses ProbeTimeout + VerifyTimeout + 2m.
	Timeout time.Duration
}

// activationRunner is the validated, seam-carrying form of ActivationConfig.
// The func fields default to the real traefik functions; tests replace them.
type activationRunner struct {
	cfg ActivationConfig

	write      func(dir string, r traefik.EnvironmentRoute) (traefik.WriteResult, error)
	readDigest func(dir, environmentID string) (string, bool, error)
	probe      func(ctx context.Context, cfg traefik.ProbeConfig) (traefik.ProbeResult, error)

	activateAttempts int
	activateBackoff  time.Duration
	verifyInterval   time.Duration
	saveBackoff      time.Duration
	reportAttempts   int
	reportBackoff    time.Duration
	timeout          time.Duration
	verifyTimeout    time.Duration
	logger           *log.Logger
}

// ValidateActivationTimeout rejects an explicit overall activation deadline
// that leaves less than 30s beyond the probe and verify budgets. A verify
// timeout <= 0 means the default. It is exported so cmd/agent can reject the
// flag at startup with the same rule NewExecutor enforces.
func ValidateActivationTimeout(activation, probe, verify time.Duration) error {
	if verify <= 0 {
		verify = defaultVerifyTimeout
	}
	if activation < probe+verify+minTimeoutMargin {
		return errors.New("agent executor: activation timeout must be at least probe timeout + verify timeout + 30s")
	}
	return nil
}

func newActivationRunner(cfg ActivationConfig, logger *log.Logger) (*activationRunner, error) {
	if cfg.Client == nil || cfg.Observer == nil || cfg.Store == nil || cfg.Revisions == nil || cfg.Drains == nil ||
		cfg.RouteDir == "" || cfg.ProbeAddress == "" || cfg.ProbeTimeout <= 0 {
		return nil, errors.New("agent executor: incomplete activation configuration")
	}
	verifyTimeout := cfg.VerifyTimeout
	if verifyTimeout <= 0 {
		verifyTimeout = defaultVerifyTimeout
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = cfg.ProbeTimeout + verifyTimeout + activationSlack
	} else if err := ValidateActivationTimeout(timeout, cfg.ProbeTimeout, verifyTimeout); err != nil {
		return nil, err
	}
	return &activationRunner{
		cfg:              cfg,
		write:            traefik.Write,
		readDigest:       traefik.ReadDigest,
		probe:            traefik.ProbeDataPlane,
		activateAttempts: defaultActivateAttempts,
		activateBackoff:  defaultActivateBackoff,
		verifyInterval:   defaultVerifyInterval,
		saveBackoff:      defaultSaveBackoff,
		reportAttempts:   defaultReportAttempts,
		reportBackoff:    defaultReportBackoff,
		timeout:          timeout,
		verifyTimeout:    verifyTimeout,
		logger:           logger,
	}, nil
}

// activate runs the sequence for a lease whose health gate passed.
func (e *Executor) activate(ctx context.Context, lease agentclient.Lease, imageDigestStr string) agentclient.Outcome {
	a := e.activation
	imageDigest, ok := parseImageDigest(imageDigestStr)
	if !ok {
		// The build result is malformed: nothing was authorized.
		return e.outcome(lease, false, queue.FailureClassCorruptState)
	}
	if ctx.Err() != nil {
		return e.outcome(lease, false, queue.FailureClassTransient)
	}

	// Structural step lines only (fixed formats, enum/numeric arguments; never
	// error text). ctx carries the attempt's shipper (nil-safe when absent).
	logs := logship.FromContext(ctx)
	logs.Linef(logship.StreamActivation, "activation: requesting authorization")

	resp, err := a.authorize(ctx, lease, imageDigest)
	if err != nil {
		if class, definite := classifyActivateRefusal(err); definite {
			logs.Linef(logship.StreamActivation, "activation: refused by the control plane (%s)", class)
			a.logger.Printf("agent activation refused by the control plane (attempt %d): %v", lease.AttemptID, err)
			return e.outcome(lease, false, class)
		}
		logs.Linef(logship.StreamActivation, "activation: authorization state unknown; route untouched")
		a.logger.Printf("agent activation ERROR attempt %d: authorization state UNKNOWN after %d tries; route untouched, sending no completion (the attempt will expire and an operator may need to declare not_activated): %v",
			lease.AttemptID, a.activateAttempts, err)
		return agentclient.Outcome{AuthorizationUnknown: true, OperationID: fmt.Sprintf("op-%d-%d", lease.AttemptID, lease.Fence)}
	}

	// From here on the switch is authorized: lease cancellation or a stale
	// renewal must not abandon it half-done.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.timeout)
	defer cancel()

	logs.Linef(logship.StreamActivation, "activation: authorized (route revision %d)", resp.RouteRevision)
	rec := state.PendingActivation{
		AttemptID:     lease.AttemptID,
		Fence:         lease.Fence,
		EnvironmentID: lease.EnvironmentID,
		OperationID:   resp.OperationID,
		RouteDigest:   resp.RouteDigest,
		ImageDigest:   imageDigest,
		RouteRevision: resp.RouteRevision,
		Phase:         state.PhaseAuthorized,

		ReleaseID:                    lease.ReleaseID,
		BackendRuntimeInstanceID:     resp.BackendRuntimeInstanceID,
		PredecessorReleaseID:         resp.PredecessorReleaseID,
		PredecessorRuntimeInstanceID: resp.PredecessorRuntimeInstanceID,
		DrainSeconds:                 lease.DrainSeconds,
	}
	if err := a.cfg.Store.SaveActivation(rec); err != nil {
		a.logger.Printf("agent activation ERROR attempt %d: cannot persist the authorized record, route untouched: %v", lease.AttemptID, err)
		if res, rerr := agentclient.ActivationFailed(boundSummary("host untouched: the agent could not persist the authorized activation record, so the route was not written")); rerr == nil {
			a.reportDirect(dctx, rec, res)
		}
		return agentclient.Outcome{AuthorizationUnknown: true, OperationID: resp.OperationID}
	}

	observed, summary := a.switchRoute(dctx, lease, resp)
	rec.Phase = state.PhaseResultReady
	if summary == "" {
		logs.Linef(logship.StreamActivation, "activation: route switched and verified")
		rec.Succeeded = true
		rec.ObservedRouteDigest = observed
	} else {
		rec.FailureSummary = summary
		logs.Linef(logship.StreamActivation, "activation: route switch failed; see the activation result")
		a.logger.Printf("agent activation ERROR attempt %d: %s", lease.AttemptID, summary)
	}
	if err := a.saveWithRetry(rec); err != nil {
		// The record is still `authorized`, and the reporter intentionally
		// ignores `authorized` records (the executor may still own them), so
		// it has nothing to send. Report the REAL result directly, bounded.
		// If that also fails, the `authorized` record stays on disk and
		// startup recovery converts it into a result_ready failure with
		// on-disk evidence for the reporter.
		a.logger.Printf("agent activation ERROR attempt %d: cannot persist the result record after retries: %v", lease.AttemptID, err)
		// A 200 acknowledgement of a success with a predecessor must first
		// make the drain durable. If that fails the authorized record stays:
		// startup recovery then converts it into a failure report (the server
		// answers that re-send terminally) and the drain is LOST. Accepted
		// and logged: a stop obligation never outranks the report, and the
		// predecessor keeps running until an operator stops it.
		terminal, acked := a.reportDirect(dctx, rec, resultFromRecord(rec))
		mayDelete := terminal
		if terminal && acked && !a.armDrainWithRetry(rec) {
			mayDelete = false
		}
		if mayDelete {
			// The server holds the report or a final decision: drop the
			// leftover `authorized` record so recovery cannot later re-report
			// it as a failure. If the delete fails, recovery converts it to a
			// failure report; the server already holds this attempt's result,
			// so ReportActivationResult answers that re-send terminally: an
			// identical replay -> 200, a report contradicting a succeeded or
			// failed operation -> integrity_conflict, any report on a
			// reconciliation_required operation -> late_report_recorded
			// (store/postgres ReportActivationResult; traced in 10d-4 review).
			if derr := a.cfg.Store.DeleteActivation(rec.AttemptID); derr != nil {
				a.logger.Printf("agent activation ERROR attempt %d: cannot delete the authorized record after a terminal direct report; startup recovery will re-report it and the server will answer terminally: %v", lease.AttemptID, derr)
			}
		}
	}
	logs.Linef(logship.StreamActivation, "activation: handed off for result reporting")
	if a.cfg.Kicker != nil {
		a.cfg.Kicker.Kick()
	}
	return agentclient.Outcome{HandedOff: true, OperationID: resp.OperationID}
}

// saveWithRetry saves rec, retrying a few times with short backoff.
func (a *activationRunner) saveWithRetry(rec state.PendingActivation) error {
	var err error
	backoff := a.saveBackoff
	for i := 0; i < defaultSaveAttempts; i++ {
		if i > 0 {
			time.Sleep(backoff)
			backoff *= 2
		}
		if err = a.cfg.Store.SaveActivation(rec); err == nil {
			return nil
		}
	}
	return err
}

// resultFromRecord builds the report for a result_ready record: success with
// the on-disk digest, or failure with the summary.
func resultFromRecord(rec state.PendingActivation) agentclient.ActivationResult {
	if rec.Succeeded {
		return agentclient.ActivationSucceeded(rec.ObservedRouteDigest)
	}
	res, err := agentclient.ActivationFailed(boundSummary(rec.FailureSummary))
	if err != nil {
		res, _ = agentclient.ActivationFailed("activation failed; summary unavailable")
	}
	return res
}

// authorize calls Activate, replaying it while the answer is unknown.
func (a *activationRunner) authorize(ctx context.Context, lease agentclient.Lease, imageDigest [32]byte) (agentclient.ActivateResponse, error) {
	var lastErr error
	sawUnknown := false
	backoff := a.activateBackoff
	for i := 0; i < a.activateAttempts; i++ {
		if i > 0 {
			t := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				t.Stop()
				return agentclient.ActivateResponse{}, fmt.Errorf("%w (last: %v)", ctx.Err(), lastErr)
			case <-t.C:
			}
			backoff *= 2
		}
		resp, err := a.cfg.Client.Activate(ctx, lease.AttemptID, lease.Fence, imageDigest)
		if err == nil {
			return resp, nil
		}
		if _, definite := classifyActivateRefusal(err); definite {
			if sawUnknown {
				// An earlier try's answer was lost: it may have committed tx1,
				// so this 409 (activation_open, candidate_not_ready, stale...)
				// may be a consequence of our own authorization. Not definite.
				// Deliberately not wrapped (%v): callers must not classify it.
				return agentclient.ActivateResponse{}, fmt.Errorf("a later try answered %q after an earlier unknown answer (last unknown: %v)", err.Error(), lastErr)
			}
			return agentclient.ActivateResponse{}, err
		}
		sawUnknown = true
		lastErr = err
		a.logger.Printf("agent activation attempt %d: Activate answer unknown (try %d/%d): %v", lease.AttemptID, i+1, a.activateAttempts, err)
	}
	return agentclient.ActivateResponse{}, lastErr
}

// classifyActivateRefusal reports whether err is a DEFINITE refusal (a 409,
// returned before anything was authorized) and, if so, the failure class.
// Any other error leaves the authorization state unknown.
//
// Mapping (conservative; only transient is retried by the control plane):
//   - stale_lease (ErrStaleLease): corrupt_state, as classifyPhaseError does.
//   - candidate_not_ready: transient (the candidate may become ready).
//   - no_route_head: permanent_configuration (no hostname assigned).
//   - every other code, an empty or unknown code: corrupt_state, the
//     non-retryable class classifyPhaseError uses for ErrIntegrityConflict.
func classifyActivateRefusal(err error) (queue.FailureClass, bool) {
	if errors.Is(err, agentclient.ErrStaleLease) {
		return queue.FailureClassCorruptState, true
	}
	code, ok := agentclient.ConflictCode(err)
	if !ok {
		return "", false
	}
	switch code {
	case agentclient.ConflictCandidateNotReady:
		return queue.FailureClassTransient, true
	case agentclient.ConflictNoRouteHead:
		return queue.FailureClassPermanentConfiguration, true
	default:
		return queue.FailureClassCorruptState, true
	}
}

// reportDirect sends result straight to the control plane when no
// result_ready record could be persisted: bounded retries on its own
// detached deadline (ctx's cancellation and expiry are ignored), stopping at
// any answer the reporter would treat as terminal. It reports whether such a
// terminal answer was received (terminal) and whether it was a 200
// acknowledgement (acked; a terminal 409 is not one).
func (a *activationRunner) reportDirect(ctx context.Context, rec state.PendingActivation, result agentclient.ActivationResult) (terminal, acked bool) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), directReportTimeout)
	defer cancel()
	backoff := a.reportBackoff
	var err error
	for i := 0; i < a.reportAttempts; i++ {
		if i > 0 {
			t := time.NewTimer(backoff)
			select {
			case <-rctx.Done():
				t.Stop()
			case <-t.C:
			}
			if rctx.Err() != nil {
				break
			}
			backoff = min(backoff*2, maxReportBackoff)
		}
		if err = a.cfg.Client.ReportActivationResult(rctx, rec.AttemptID, rec.Fence, rec.OperationID, result); agentclient.ReportAnswerIsTerminal(err) {
			return true, err == nil
		}
	}
	a.logger.Printf("agent activation ERROR attempt %d: direct report did not land (last: %v); the authorized record stays on disk for startup recovery to convert", rec.AttemptID, err)
	return false, false
}

// armDrainWithRetry retries armDrain a few times with short backoff, since a
// failed arm (fsync, ENOSPC) is often transient.
func (a *activationRunner) armDrainWithRetry(rec state.PendingActivation) bool {
	backoff := a.saveBackoff
	for i := 0; i < defaultSaveAttempts; i++ {
		if i > 0 {
			time.Sleep(backoff)
			backoff *= 2
		}
		if a.armDrain(rec) {
			return true
		}
	}
	return false
}

// armDrain persists the predecessor drain of an acknowledged success. It
// reports whether the caller may delete the authorized record. newActivationRunner rejects a
// missing drain store, so the nil branch is a defensive guard that logs every
// lost drain at ERROR.
func (a *activationRunner) armDrain(rec state.PendingActivation) bool {
	d, owed := state.DrainFromActivation(rec, time.Now())
	if !owed {
		return true
	}
	if a.cfg.Drains == nil {
		a.logger.Printf("agent activation ERROR attempt %d: a predecessor drain is owed but no drain store is configured; the predecessor will not be stopped", rec.AttemptID)
		return true
	}
	if err := a.cfg.Drains.ArmDrain(d); err != nil {
		a.logger.Printf("agent activation ERROR attempt %d: report acknowledged but the predecessor drain could not be armed; keeping the authorized record: %v", rec.AttemptID, err)
		return false
	}
	return true
}

// switchRoute runs re-render, revision guard, write, verify and probe. It
// returns the on-disk digest on success, or a non-empty failure summary.
// It never rolls back.
func (a *activationRunner) switchRoute(ctx context.Context, lease agentclient.Lease, resp agentclient.ActivateResponse) ([32]byte, string) {
	authorized := routeDigestString(resp.RouteDigest)
	fail := func(step string, reason error) ([32]byte, string) {
		return [32]byte{}, a.failureSummary(step, reason.Error(), authorized, lease.EnvironmentID)
	}

	if resp.EntrypointName != traefik.DefaultEntrypointName || resp.CertResolverName != traefik.DefaultCertResolverName {
		return fail("entrypoint-check", fmt.Errorf("control plane named entrypoint %q / cert resolver %q, this agent requires %q / %q",
			resp.EntrypointName, resp.CertResolverName, traefik.DefaultEntrypointName, traefik.DefaultCertResolverName))
	}
	route := traefik.EnvironmentRoute{
		EnvironmentID:            lease.EnvironmentID,
		RouteRevision:            uint64(resp.RouteRevision),
		Hostname:                 resp.Hostname,
		EntrypointName:           resp.EntrypointName,
		CertResolverName:         resp.CertResolverName,
		BackendRuntimeInstanceID: resp.BackendRuntimeInstanceID,
		BackendPort:              lease.ContainerPort,
		BackendScheme:            lease.ContainerScheme,
		ReleaseID:                lease.ReleaseID,
		FenceID:                  strconv.FormatInt(lease.Fence, 10),
	}
	_, rendered, err := traefik.Render(route)
	if err != nil {
		return fail("render", err)
	}
	if rendered != authorized {
		return fail("render", fmt.Errorf("agent rendered digest %s differs from the authorized digest", rendered))
	}

	stored, err := a.cfg.Revisions.LoadRouteRevision(lease.EnvironmentID)
	if err != nil {
		return fail("revision-guard", fmt.Errorf("load stored route revision: %w", err))
	}
	if resp.RouteRevision < stored {
		return fail("revision-guard", fmt.Errorf("authorized route revision %d is below the stored revision %d", resp.RouteRevision, stored))
	}
	// Record BEFORE writing: a crash after the write must still leave the
	// guard advanced.
	if err := a.cfg.Revisions.RecordRouteRevision(lease.EnvironmentID, resp.RouteRevision); err != nil {
		return fail("revision-guard", fmt.Errorf("record route revision %d: %w", resp.RouteRevision, err))
	}

	if _, err := a.write(a.cfg.RouteDir, route); err != nil {
		return fail("route-write", err)
	}
	if err := a.verify(ctx, route); err != nil {
		return fail("observer-verify", err)
	}
	probeRes, err := a.probe(ctx, traefik.ProbeConfig{
		Address:  a.cfg.ProbeAddress,
		Hostname: resp.Hostname,
		Path:     a.cfg.ProbePath,
		Timeout:  a.cfg.ProbeTimeout,
	})
	if err != nil {
		return fail("data-plane-probe", fmt.Errorf("%w (attempts=%d last_status=%d reason=%s)", err, probeRes.Attempts, probeRes.LastStatus, probeRes.Reason))
	}

	onDisk, exists, err := a.readDigest(a.cfg.RouteDir, lease.EnvironmentID)
	if err != nil || !exists {
		return fail("on-disk-check", errors.New("route file is not readable after the switch"))
	}
	if onDisk != authorized {
		return fail("on-disk-check", errors.New("on-disk route digest differs from the authorized digest"))
	}
	digest, ok := parseRouteDigestString(onDisk)
	if !ok {
		return fail("on-disk-check", errors.New("on-disk digest is malformed"))
	}
	return digest, ""
}

// verify polls Observer.Verify while Traefik has not finished its
// asynchronous reload (errors classified transient), within verifyTimeout.
func (a *activationRunner) verify(ctx context.Context, route traefik.EnvironmentRoute) error {
	vctx, cancel := context.WithTimeout(ctx, a.verifyTimeout)
	defer cancel()
	for {
		_, err := a.cfg.Observer.Verify(vctx, route)
		if err == nil {
			return nil
		}
		if traefik.ClassifyObservation(err) != queue.FailureClassTransient {
			return err
		}
		t := time.NewTimer(a.verifyInterval)
		select {
		case <-vctx.Done():
			t.Stop()
			return fmt.Errorf("%w (last: %v)", vctx.Err(), err)
		case <-t.C:
		}
	}
}

// failureSummary builds a bounded, valid-UTF-8 failure summary carrying the
// failed step, the reason and the on-disk vs authorized digest evidence.
func (a *activationRunner) failureSummary(step, reason, authorized, environmentID string) string {
	onDisk := "unreadable"
	if d, exists, err := a.readDigest(a.cfg.RouteDir, environmentID); err == nil {
		if exists {
			onDisk = d
		} else {
			onDisk = "absent"
		}
	}
	return buildFailureSummary(step, reason, onDisk, authorized)
}

// buildFailureSummary keeps the step and digest evidence intact and gives the
// free-text reason whatever room is left (at most maxReasonBytes), so a long
// reason can never push the digests past the 2048 byte bound.
func buildFailureSummary(step, reason, onDisk, authorized string) string {
	const format = "activation failed at %s: %s; on-disk route digest: %s; authorized route digest: %s; route not rolled back"
	fixed := len(fmt.Sprintf(format, step, "", onDisk, authorized))
	budget := min(maxReasonBytes, max(maxFailureSummaryBytes-fixed, 0))
	return boundSummary(fmt.Sprintf(format, step, truncateUTF8(reason, budget), onDisk, authorized))
}

func boundSummary(s string) string { return truncateUTF8(s, maxFailureSummaryBytes) }

// truncateUTF8 returns s made valid UTF-8 and cut to at most max bytes on a
// rune boundary.
func truncateUTF8(s string, max int) string {
	s = strings.ToValidUTF8(s, "?")
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func routeDigestString(d [32]byte) string { return "sha256:" + hex.EncodeToString(d[:]) }

func parseRouteDigestString(s string) ([32]byte, bool) {
	return parseHex32(strings.TrimPrefix(s, imageDigestPrefix), strings.HasPrefix(s, imageDigestPrefix))
}

// parseImageDigest accepts the build result's "sha256:<64 hex>" form.
func parseImageDigest(s string) ([32]byte, bool) {
	return parseHex32(strings.TrimPrefix(s, imageDigestPrefix), true)
}

func parseHex32(h string, prefixOK bool) ([32]byte, bool) {
	var d [32]byte
	if !prefixOK || len(h) != hex.EncodedLen(len(d)) {
		return d, false
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return d, false
	}
	copy(d[:], b)
	return d, true
}
