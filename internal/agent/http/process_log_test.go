package agenthttp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"autodeploy/internal/githubapp"
	"autodeploy/internal/testutil/srcguard"

	"github.com/jackc/pgx/v5/pgconn"
)

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

func leakyCheckoutError() error {
	return fmt.Errorf("dial postgres://u:hunter2-dsn@db/x with ghs_fixtureTOKEN123: %w", &pgconn.PgError{Code: "40001"})
}

func assertCheckoutLog(t *testing.T, buf *bytes.Buffer, want, class string) {
	t.Helper()
	out := buf.String()
	if !strings.Contains(out, want) || !strings.Contains(out, class) {
		t.Errorf("log %q missing %q or class", out, want)
	}
	for _, secret := range []string{"hunter2-dsn", "ghs_fixtureTOKEN123", "postgres://"} {
		if strings.Contains(out, secret) {
			t.Errorf("log %q leaks %q", out, secret)
		}
	}
}

func startCheckout(t *testing.T, f *checkoutFixture, buf *bytes.Buffer) int {
	t.Helper()
	h := f.handler()
	h.Logger = plainLogger(buf)
	r := newRequestTLS(newRequest(t, pathCheckoutStart, checkoutStartBody(fixtureAttemptID, fixtureFence, fixtureReservID), checkoutTestCredential(t), "server_1"))
	return do(h, r).Code
}

// failingActivator returns an error whose text carries fixture secrets and
// whose chain carries a SQLSTATE, so the handler's own log line (not
// githubapp's error flattening) decides what is logged.
type failingActivator struct{ err error }

func (a failingActivator) Activate(context.Context, string) (*githubapp.CheckoutLease, error) {
	return nil, a.err
}

func TestCheckoutStartActivateFailureLogsClassNotText(t *testing.T) {
	f := beginThenReturnFixture(t, "server_1")
	var buf bytes.Buffer
	h := f.handler()
	h.Logger = plainLogger(&buf)
	h.CheckoutActivator = failingActivator{err: leakyCheckoutError()}
	r := newRequestTLS(newRequest(t, pathCheckoutStart, checkoutStartBody(fixtureAttemptID, fixtureFence, fixtureReservID), checkoutTestCredential(t), "server_1"))
	if code := do(h, r).Code; code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", code)
	}
	assertCheckoutLog(t, &buf, "agent checkout start failed", "class=postgres_40001")
	if got := buf.String(); got != "level=ERROR msg=\"agent checkout start failed\" stage=activate class=postgres_40001\n" {
		t.Fatalf("log line = %q", got)
	}
}

// CheckoutActivator returns the concrete *githubapp.CheckoutLease, whose Start
// always flattens a store failure to ErrCheckoutStale, so a postgres-class
// error cannot reach this step. The exact-line assertion still fails if the
// error text is ever appended to the line.
func TestCheckoutStartLeaseStartFailureLogsExactLine(t *testing.T) {
	f := beginThenReturnFixture(t, "server_1")
	f.store.startErr = leakyCheckoutError()
	var buf bytes.Buffer
	if code := startCheckout(t, f, &buf); code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", code)
	}
	assertCheckoutLog(t, &buf, "agent checkout start failed", "class=other")
	if got := buf.String(); got != "level=ERROR msg=\"agent checkout start failed\" stage=\"lease start\" class=other\n" {
		t.Fatalf("log line = %q", got)
	}
}

// The delivery step is only reachable when a started lease is invalidated
// between Start and DeliverToken, which cannot be forced deterministically
// through the concrete lease, so the shared logging helper is exercised directly.
func TestCheckoutStartFailureHelperLogsClassNotText(t *testing.T) {
	for _, step := range []string{"activate", "lease start", "token delivery"} {
		var buf bytes.Buffer
		Handler{Logger: plainLogger(&buf)}.logCheckoutStartFailure(step, leakyCheckoutError())
		assertCheckoutLog(t, &buf, "agent checkout start failed", "class=postgres_40001")
	}
}

func TestUnsetLoggerDiscards(t *testing.T) {
	if l := (Handler{}).logger(); l.Handler() != slog.DiscardHandler || l.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("an unset logger must discard, never write to standard error")
	}
}

func TestProductionSourceHasNoDirectProcessOutput(t *testing.T) {
	srcguard.CheckPackageDir(t, ".")
}
