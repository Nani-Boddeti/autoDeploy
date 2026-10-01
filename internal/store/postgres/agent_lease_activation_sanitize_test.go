package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"unicode/utf8"

	"autodeploy/internal/deployment"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestSanitizeOutcomeSummary(t *testing.T) {
	if got := sanitizeOutcomeSummary("  reload\x00 failed\n\x7f ok\u0085 "); got != "reload  failed   ok" {
		t.Fatalf("got %q", got)
	}
	if got := sanitizeOutcomeSummary("bad\xffutf8"); got != "bad?utf8" {
		t.Fatalf("got %q", got)
	}
	// The 512-byte cut must land MID-rune or the back-off loop is never
	// exercised: 400 two-byte runes alone would cut exactly on a boundary.
	// One leading ASCII byte shifts every two-byte rune to an odd offset, so
	// byte 512 is a continuation byte; a three-byte rune repeated cuts after
	// its second byte (512 = 170*3 + 2).
	for name, tc := range map[string]struct {
		in   string
		want int
	}{
		"two-byte runes offset by one ASCII byte": {"a" + strings.Repeat("é", 400), 511},
		"three-byte runes":                        {strings.Repeat("€", 400), 510},
	} {
		got := sanitizeOutcomeSummary(tc.in)
		if len(got) != tc.want || !utf8.ValidString(got) || !strings.HasPrefix(tc.in, got) {
			t.Fatalf("%s: len=%d valid=%v, want a valid rune-boundary prefix of %d bytes", name, len(got), utf8.ValidString(got), tc.want)
		}
	}
	if got := sanitizeOutcomeSummary(""); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestClassifyActivationResultError(t *testing.T) {
	wrapped := func(code string) error {
		return fmt.Errorf("commit: %w", &pgconn.PgError{Code: code, ConstraintName: "some_constraint"})
	}
	for _, code := range []string{"23505", "23503", "23514", "P0001"} {
		if err := classifyActivationResultError(wrapped(code)); !errors.Is(err, ErrAgentLeaseConstraintViolation) {
			t.Fatalf("%s classified as %v, want ErrAgentLeaseConstraintViolation", code, err)
		}
	}
	// Deadlocks, serialization failures and cancellations stay retryable.
	for _, code := range []string{"40P01", "40001", "57014", "08006"} {
		if err := classifyActivationResultError(wrapped(code)); errors.Is(err, ErrAgentLeaseConstraintViolation) {
			t.Fatalf("%s must stay unclassified", code)
		}
	}
	if err := classifyActivationResultError(context.DeadlineExceeded); err != context.DeadlineExceeded {
		t.Fatalf("deadline classified as %v", err)
	}
	if err := classifyActivationResultError(fmt.Errorf("x: %w", deployment.ErrInvalidTransition)); !errors.Is(err, ErrAgentLeaseCorruptState) {
		t.Fatalf("invalid transition classified as %v", err)
	}
	if classifyActivationResultError(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	// The deployment repository's sentinels cannot arise from concurrency
	// inside tx2 (the deployment row is locked): they mean corruption.
	for name, sentinel := range map[string]error{"revision conflict": ErrRevisionConflict, "not found": ErrNotFound, "revision overflow": ErrRevisionOverflow} {
		if err := classifyActivationResultError(fmt.Errorf("x: %w", sentinel)); !errors.Is(err, ErrAgentLeaseCorruptState) {
			t.Fatalf("%s classified as %v, want ErrAgentLeaseCorruptState", name, err)
		}
	}
	// A block that could not be written is never terminal, even when the
	// wrapped error is a deterministic PgError.
	if err := classifyActivationResultError(blockUnrecorded(&pgconn.PgError{Code: "23514"})); errors.Is(err, ErrAgentLeaseConstraintViolation) || !errors.Is(err, ErrActivationBlockUnrecorded) {
		t.Fatalf("unrecorded block classified as %v, want it left retryable", err)
	}
	// 0A000 is transient only for the stale statement cache, identified by the
	// language-independent source routine (not the English message).
	if err := classifyActivationResultError(&pgconn.PgError{Code: "0A000", Routine: "RevalidateCachedQuery", Message: "plan de cache localise"}); errors.Is(err, ErrAgentLeaseConstraintViolation) {
		t.Fatalf("stale statement cache classified as %v, want retryable", err)
	}
	if err := classifyActivationResultError(&pgconn.PgError{Code: "0A000", Message: "cached plan must not change result type", Routine: "other"}); !errors.Is(err, ErrAgentLeaseConstraintViolation) {
		t.Fatalf("other 0A000 classified as %v, want terminal", err)
	}
}

// TestIsDeterministicActivationFailure pins the rule's boundary. The
// definition is NEGATIVE: every error that is not on the known-transient list
// is recorded as a reconciliation block (an unrecognised error after the
// occupant check must block, not retry forever with the operation silently
// open). The known-transient list is the only list that must be right.
func TestIsDeterministicActivationFailure(t *testing.T) {
	pg := func(code string) error { return fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: code}) }
	for name, err := range map[string]error{
		"unique violation":            pg("23505"),
		"deferred fk violation":       pg("23503"),
		"trigger exception":           pg("P0001"),
		"data exception":              pg("22001"),
		"corrupt state":               fmt.Errorf("x: %w", ErrAgentLeaseCorruptState),
		"stale":                       fmt.Errorf("x: %w", ErrAgentLeaseStale),
		"integrity conflict":          fmt.Errorf("x: %w", ErrAgentLeaseIntegrityConflict),
		"route moved":                 fmt.Errorf("x: %w", ErrAgentLeaseActivationRouteMoved),
		"constraint violation":        fmt.Errorf("x: %w", ErrAgentLeaseConstraintViolation),
		"invalid deployment transit.": fmt.Errorf("x: %w", deployment.ErrInvalidTransition),
		"deployment terminal state":   fmt.Errorf("x: %w", deployment.ErrTerminalState),
		"revision conflict":           fmt.Errorf("x: %w", ErrRevisionConflict),
		"deployment not found":        fmt.Errorf("x: %w", ErrNotFound),
		"revision overflow":           fmt.Errorf("x: %w", ErrRevisionOverflow),
		// NEGATIVE definition: anything not known to be transient blocks.
		"unknown plain error":     errors.New("something nobody anticipated"),
		"unrecognised read error": errors.New("read tcp: connection reset"),
		"feature not supported":   &pgconn.PgError{Code: "0A000", Message: "unrelated unsupported feature"},
		"syntax error":            pg("42601"),
	} {
		if !isDeterministicActivationFailure(err) {
			t.Fatalf("%s must be deterministic", name)
		}
	}
	for name, err := range map[string]error{
		"nil":                              nil,
		"serialization":                    pg("40001"),
		"deadlock":                         pg("40P01"),
		"query canceled":                   pg("57014"),
		"connection failure":               pg("08006"),
		"connection exception":             pg("08003"),
		"admin shutdown":                   pg("57P01"),
		"out of memory":                    pg("53200"),
		"lock not available":               pg("55P03"),
		"context canceled":                 fmt.Errorf("x: %w", context.Canceled),
		"context deadline":                 fmt.Errorf("x: %w", context.DeadlineExceeded),
		"read-only transaction (failover)": pg("25006"),
		"idle in transaction timeout":      pg("25P03"),
		"transaction timeout":              pg("25P04"),
		"idle session timeout":             pg("57P05"),
		"io error":                         pg("58030"),
		"statement completion unknown":     pg("40003"),
		"invalid sql statement name":       pg("26000"),
		"cached plan result type changed":  fmt.Errorf("x: %w", &pgconn.PgError{Code: "0A000", Routine: "RevalidateCachedQuery"}),
		"internal error XX000":             pg("XX000"),
		"conn closed":                      fmt.Errorf("x: %w", pgconn.ErrConnClosed),
		"io EOF":                           fmt.Errorf("x: %w", io.EOF),
		"unexpected EOF":                   fmt.Errorf("x: %w", io.ErrUnexpectedEOF),
		"network timeout":                  &net.OpError{Op: "read", Err: errors.New("i/o timeout")},
	} {
		if isDeterministicActivationFailure(err) {
			t.Fatalf("%s must not be deterministic (it is known-transient)", name)
		}
	}
	// A non-transient SQLSTATE outside class 23/P0001 is still surfaced as the
	// typed terminal error, never left looking retryable.
	if err := classifyActivationResultError(pg("22001")); !errors.Is(err, ErrAgentLeaseConstraintViolation) {
		t.Fatalf("22001 classified as %v, want ErrAgentLeaseConstraintViolation", err)
	}
	for _, code := range []string{"53200", "57P01", "55P03", "25006", "58030", "26000", "40003"} {
		if err := classifyActivationResultError(pg(code)); errors.Is(err, ErrAgentLeaseConstraintViolation) {
			t.Fatalf("%s must stay unclassified", code)
		}
	}
}
