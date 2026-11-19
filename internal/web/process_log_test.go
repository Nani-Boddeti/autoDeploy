package web

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"autodeploy/internal/testutil/srcguard"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	fixtureDSN   = "postgres://u:hunter2-dsn@db/x"
	fixtureToken = "ghs_fixtureTOKEN123"
)

// leakyError carries a DSN and a token in its text and a SQLSTATE in its chain.
func leakyError() error {
	return fmt.Errorf("dial %s with %s: %w", fixtureDSN, fixtureToken, &pgconn.PgError{Code: "40001"})
}

func assertLogged(t *testing.T, buf *bytes.Buffer, wants ...string) {
	t.Helper()
	out := buf.String()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("log %q missing %q", out, w)
		}
	}
	for _, secret := range []string{"hunter2-dsn", "ghs_fixtureTOKEN123", "postgres://"} {
		if strings.Contains(out, secret) {
			t.Errorf("log %q leaks %q", out, secret)
		}
	}
}

// plainLogger is an unredacted text logger: the tests prove the call sites
// themselves never log error text, so redaction must not mask a leak.
func plainLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}))
}

func TestRollbackEligibilityFailureLogsClassNotText(t *testing.T) {
	h, st, ops, p := rollbackHandler(t)
	var buf bytes.Buffer
	h.config.Logger = plainLogger(&buf)
	ops.detail = retainedDetail("healthy")
	st.eligibleErr = leakyError()
	if rr := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1"); rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rr.Code)
	}
	assertLogged(t, &buf, "rollback eligibility failed", "class=postgres_40001")
}

func TestRollbackFailureLogsClassNotText(t *testing.T) {
	h, st, _, p := rollbackHandler(t)
	var buf bytes.Buffer
	h.config.Logger = plainLogger(&buf)
	st.enqueueErr = leakyError()
	if rr := cancelPost(h, p, "/deployments/rollback", rollbackForm(t, h, p), nil); rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rr.Code)
	}
	assertLogged(t, &buf, "rollback failed", "class=postgres_40001")
}

func TestManualDeployFailureLogsStageAndClassNotText(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   func(*manualStoreStub, *resolverStub)
		stage string
	}{
		{"target", func(s *manualStoreStub, _ *resolverStub) { s.targetErr = leakyError() }, "target lookup"},
		{"resolve", func(_ *manualStoreStub, r *resolverStub) { r.err = leakyError() }, "commit resolution (GitHub)"},
		{"enqueue", func(s *manualStoreStub, _ *resolverStub) { s.enqueueErr = leakyError() }, "enqueue"},
	} {
		h, st, rs, p := manualHandler(t)
		var buf bytes.Buffer
		h.config.Logger = plainLogger(&buf)
		tc.set(st, rs)
		if rr := cancelPost(h, p, "/deployments/manual", manualForm(t, h, p), nil); rr.Code != http.StatusInternalServerError {
			t.Fatalf("%s status = %d", tc.name, rr.Code)
		}
		assertLogged(t, &buf, "manual deploy failed", "stage="+textValue(tc.stage), "class=postgres_40001")
	}
}

func TestGitHubSetupFailuresLogStageOnly(t *testing.T) {
	h, store, p := githubHandler(t)
	store.reserved = 1 // the stub refuses a second reservation, failing the callback
	var buf bytes.Buffer
	h.config.Logger = plainLogger(&buf)
	state := strings.Repeat("A", 43)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://admin.example.test/github/setup/callback?state="+state+"&installation_id=11&setup_action=install", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("callback status = %d", rr.Code)
	}
	assertLogged(t, &buf, "github setup callback failed", "stage=")

	buf.Reset()
	form := url.Values{"csrf": {githubCSRF(t, h, p)}}
	r := githubRequest(http.MethodPost, "/github/setup/complete", form, p, h)
	r.AddCookie(&http.Cookie{Name: handoffCookieName, Value: strings.Repeat("B", 43)})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("complete status = %d", rr.Code)
	}
	assertLogged(t, &buf, "github setup complete failed", "stage=")
}

func TestUnsetLoggerDiscards(t *testing.T) {
	h, st, ops, p := rollbackHandler(t)
	ops.detail = retainedDetail("healthy")
	st.eligibleErr = leakyError()
	if rr := opsGet(h, p, http.MethodGet, "/deployments/detail?id=dep-1"); rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rr.Code)
	}
	if h.logger().Handler() != slog.DiscardHandler || h.logger().Enabled(context.Background(), slog.LevelError) {
		t.Fatal("an unset logger must discard, never write to standard error")
	}
}

func TestProductionSourceHasNoDirectProcessOutput(t *testing.T) {
	srcguard.CheckPackageDir(t, ".")
}

// textValue renders v as slog's text handler does: quoted only when it needs it.
func textValue(v string) string {
	if strings.ContainsAny(v, " =\"") {
		return strconv.Quote(v)
	}
	return v
}
