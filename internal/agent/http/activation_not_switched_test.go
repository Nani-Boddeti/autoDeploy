package agenthttp

import (
	"context"
	"crypto/rand"
	"net/http"
	"strings"
	"testing"

	"autodeploy/internal/queue"
	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
)

// outcomeReporter is a fakeActivationReporter that also serves the
// not_switched outcome (slice 22-D).
type outcomeReporter struct {
	fakeActivationReporter
	outcomeCalls int
	gotOutcome   string
}

func (f *outcomeReporter) ReportActivationResultWithOutcome(_ context.Context, _ string, _ [32]byte, attemptID, fence int64, operationID string, succeeded bool, observedRouteDigest [32]byte, failureSummary, outcome string) error {
	f.outcomeCalls++
	f.gotOutcome = outcome
	f.seen.called = true
	f.seen.attemptID, f.seen.fence, f.seen.operationID = attemptID, fence, operationID
	f.seen.succeeded, f.seen.digest, f.seen.summary = succeeded, observedRouteDigest, failureSummary
	return f.err
}

func postActivationResult(t *testing.T, h Handler, body string) *errorBodyResponse {
	t.Helper()
	tok, _ := server.NewCredentialToken(rand.Reader)
	w := do(h, newRequestTLS(newRequest(t, pathActivationResult, body, bearerPrefix+string(tok), "server_1")))
	return &errorBodyResponse{code: w.Code, body: w.Body.String()}
}

type errorBodyResponse struct {
	code int
	body string
}

func TestActivationResultOutcomeShapes(t *testing.T) {
	digest := hexDigest(digestFixture(1))
	cases := []struct {
		name         string
		body         string
		reporter     func() *outcomeReporter
		wantStatus   int
		wantOutcome  string // "" = the plain method must have been used
		wantCalled   bool
		wantPlainUse bool
	}{
		{name: "not_switched with a failure is accepted",
			body:       `{"attempt_id":1,"fence":1,"operation_id":"7","succeeded":false,"failure_summary":"gate failed","outcome":"not_switched"}`,
			wantStatus: http.StatusOK, wantOutcome: "not_switched", wantCalled: true},
		{name: "old payload without outcome uses the unchanged plain method",
			body:       `{"attempt_id":1,"fence":1,"operation_id":"7","succeeded":false,"failure_summary":"gate failed"}`,
			wantStatus: http.StatusOK, wantCalled: true, wantPlainUse: true},
		{name: "old success payload is unchanged",
			body:       `{"attempt_id":1,"fence":1,"operation_id":"7","succeeded":true,"observed_route_digest":"` + digest + `"}`,
			wantStatus: http.StatusOK, wantCalled: true, wantPlainUse: true},
		{name: "empty outcome is the plain method", body: `{"attempt_id":1,"fence":1,"operation_id":"7","succeeded":false,"failure_summary":"x","outcome":""}`,
			wantStatus: http.StatusOK, wantCalled: true, wantPlainUse: true},
		{name: "unknown outcome", body: `{"attempt_id":1,"fence":1,"operation_id":"7","succeeded":false,"failure_summary":"x","outcome":"switched"}`, wantStatus: http.StatusBadRequest},
		{name: "wrong case", body: `{"attempt_id":1,"fence":1,"operation_id":"7","succeeded":false,"failure_summary":"x","outcome":"NOT_SWITCHED"}`, wantStatus: http.StatusBadRequest},
		{name: "outcome with succeeded=true", body: `{"attempt_id":1,"fence":1,"operation_id":"7","succeeded":true,"observed_route_digest":"` + digest + `","outcome":"not_switched"}`, wantStatus: http.StatusBadRequest},
		{name: "outcome with a non-string", body: `{"attempt_id":1,"fence":1,"operation_id":"7","succeeded":false,"failure_summary":"x","outcome":true}`, wantStatus: http.StatusBadRequest},
		{name: "outcome without succeeded", body: `{"attempt_id":1,"fence":1,"operation_id":"7","failure_summary":"x","outcome":"not_switched"}`, wantStatus: http.StatusBadRequest},
		{name: "not_switched without a digest-free failure (digest present)", body: `{"attempt_id":1,"fence":1,"operation_id":"7","succeeded":false,"observed_route_digest":"` + digest + `","outcome":"not_switched"}`, wantStatus: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := &outcomeReporter{}
			resp := postActivationResult(t, Handler{ActivationReporter: rep}, tc.body)
			if resp.code != tc.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", resp.code, resp.body, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusBadRequest {
				if !strings.Contains(resp.body, `"invalid_request"`) || rep.seen.called {
					t.Fatalf("body=%s called=%v, want invalid_request and no repository call", resp.body, rep.seen.called)
				}
				return
			}
			if tc.wantPlainUse && (rep.outcomeCalls != 0 || !rep.seen.called) {
				t.Fatalf("outcomeCalls=%d called=%v, want the plain ReportActivationResult", rep.outcomeCalls, rep.seen.called)
			}
			if !tc.wantPlainUse && (rep.outcomeCalls != 1 || rep.gotOutcome != tc.wantOutcome) {
				t.Fatalf("outcomeCalls=%d outcome=%q, want 1 %q", rep.outcomeCalls, rep.gotOutcome, tc.wantOutcome)
			}
		})
	}
}

// A reporter that cannot serve the outcome (an older wiring) answers the
// agent's strip-and-retry fallback with 400 invalid_request, never a block.
func TestActivationResultOutcomeWithoutServerSupportIs400(t *testing.T) {
	rep := &fakeActivationReporter{}
	resp := postActivationResult(t, Handler{ActivationReporter: rep}, `{"attempt_id":1,"fence":1,"operation_id":"7","succeeded":false,"failure_summary":"x","outcome":"not_switched"}`)
	if resp.code != http.StatusBadRequest || !strings.Contains(resp.body, `"invalid_request"`) || rep.seen.called {
		t.Fatalf("status=%d body=%s called=%v", resp.code, resp.body, rep.seen.called)
	}
}

func TestActivationResultNotSwitchedErrorsMapLikeAnyFailureReport(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"integrity conflict (replay of a finalized report)": {postgres.ErrAgentLeaseIntegrityConflict, http.StatusConflict, "integrity_conflict"},
		"late report recorded":                              {postgres.ErrAgentLeaseActivationLateReportRecorded, http.StatusConflict, "late_report_recorded"},
		"stale":                                             {postgres.ErrAgentLeaseStale, http.StatusConflict, "stale_lease"},
	} {
		t.Run(name, func(t *testing.T) {
			rep := &outcomeReporter{fakeActivationReporter: fakeActivationReporter{err: tc.err}}
			resp := postActivationResult(t, Handler{ActivationReporter: rep}, `{"attempt_id":1,"fence":1,"operation_id":"7","succeeded":false,"failure_summary":"x","outcome":"not_switched"}`)
			if resp.code != tc.status || !strings.Contains(resp.body, `"`+tc.code+`"`) {
				t.Fatalf("status=%d body=%s, want %d %s", resp.code, resp.body, tc.status, tc.code)
			}
		})
	}
}

// --- claim opt-in and echo ----------------------------------------------

const notSwitchedCapability = `"capabilities":["activation_not_switched"]`

func stsLease() queue.ClaimedLease {
	l := claimGoldenLease()
	l.ReleaseStrategy = "stop_then_start"
	return l
}

func TestClaimEchoesNotSwitchedSupportOnlyToOptedInClaims(t *testing.T) {
	const echo = `"activation_not_switched":true`
	sts, bg, rollback := stsLease(), claimGoldenLease(), stsLease()
	rollback.JobID, rollback.RollbackTargetReleaseID = 3, "rel-target"
	h := func() (Handler, *capClaimer) {
		fc := &capClaimer{fakeClaimer: fakeClaimer{leases: []queue.ClaimedLease{sts, bg, rollback}}}
		return Handler{Authenticator: &fakeAuthenticator{}, Claimer: fc}, fc
	}
	t.Run("opted in: only the stop-then-start deployment lease carries the echo", func(t *testing.T) {
		handler, _ := h()
		code, body := claimBody(t, handler, `{"limit":1,"executor_available":true,`+notSwitchedCapability+`}`)
		if code != http.StatusOK || strings.Count(body, echo) != 1 {
			t.Fatalf("status=%d echoes=%d body=%s", code, strings.Count(body, echo), body)
		}
	})
	for name, body := range map[string]string{
		"no capabilities":      `{"limit":1,"executor_available":true}`,
		"empty capabilities":   `{"limit":1,"executor_available":true,"capabilities":[]}`,
		"health_policy only":   `{"limit":1,"executor_available":true,"capabilities":["health_policy"]}`,
		"executor unavailable": `{"limit":1,"executor_available":false,` + notSwitchedCapability + `}`,
	} {
		t.Run("no echo: "+name, func(t *testing.T) {
			handler, _ := h()
			code, resp := claimBody(t, handler, body)
			if code != http.StatusOK || strings.Contains(resp, echo) {
				t.Fatalf("status=%d body=%s must not echo", code, resp)
			}
		})
	}
}

// The bytes of a claim that did not opt into not_switched stay exactly as
// before, even for a stop-then-start lease and with health_policy opted in.
func TestClaimBytesWithoutNotSwitchedOptInAreUnchangedForStopThenStart(t *testing.T) {
	fc := &fakeClaimer{leases: []queue.ClaimedLease{stsLease()}}
	code, body := claimBody(t, Handler{Authenticator: &fakeAuthenticator{}, Claimer: fc}, `{"limit":1,"executor_available":true}`)
	want := strings.Replace(claimGoldenBody, `"release_strategy":"blue_green"`, `"release_strategy":"stop_then_start"`, 1)
	if code != http.StatusOK || body != want {
		t.Fatalf("status=%d body=%s", code, body)
	}
}

// With the opt-in, the ONLY difference is the appended echo field.
func TestClaimOptInAddsExactlyTheEchoField(t *testing.T) {
	fc := &fakeClaimer{leases: []queue.ClaimedLease{stsLease()}}
	code, body := claimBody(t, Handler{Authenticator: &fakeAuthenticator{}, Claimer: fc}, `{"limit":1,"executor_available":true,`+notSwitchedCapability+`}`)
	base := strings.Replace(claimGoldenBody, `"release_strategy":"blue_green"`, `"release_strategy":"stop_then_start"`, 1)
	want := strings.Replace(base, `"literal_bindings":[]}`, `"literal_bindings":[],"activation_not_switched":true}`, 1)
	if code != http.StatusOK || body != want || fc.calls != 1 {
		t.Fatalf("status=%d calls=%d\n got  %s\n want %s", code, fc.calls, body, want)
	}
}

func TestClaimCapabilityListAcceptsBothAndRejectsUnknown(t *testing.T) {
	fc := &capClaimer{fakeClaimer: fakeClaimer{leases: []queue.ClaimedLease{stsLease()}}}
	code, body := claimBody(t, Handler{Authenticator: &fakeAuthenticator{}, Claimer: fc}, `{"limit":1,"executor_available":true,"capabilities":["health_policy","activation_not_switched"]}`)
	if code != http.StatusOK || fc.capCalls != 1 || !fc.gotCaps.HealthPolicy || !fc.gotCaps.ActivationNotSwitched || !strings.Contains(body, `"activation_not_switched":true`) || !strings.Contains(body, `"health_policy":{`) {
		t.Fatalf("status=%d capCalls=%d caps=%+v body=%s", code, fc.capCalls, fc.gotCaps, body)
	}
	if code, _ = claimBody(t, Handler{Authenticator: &fakeAuthenticator{}, Claimer: fc}, `{"limit":1,"executor_available":true,"capabilities":["activation_not_switched_v2"]}`); code != http.StatusBadRequest {
		t.Fatalf("unknown capability status=%d, want 400", code)
	}
}
