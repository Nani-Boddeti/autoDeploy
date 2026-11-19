package errclass

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestOf(t *testing.T) {
	const dsn = "postgres://u:hunter2-dsn@db/x"
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "none"},
		{"canceled", context.Canceled, "context_canceled"},
		{"wrapped canceled", fmt.Errorf("%s: %w", dsn, context.Canceled), "context_canceled"},
		{"deadline", context.DeadlineExceeded, "deadline_exceeded"},
		{"wrapped deadline", fmt.Errorf("x: %w", context.DeadlineExceeded), "deadline_exceeded"},
		{"pg", &pgconn.PgError{Code: "40001", Message: dsn}, "postgres_40001"},
		{"wrapped pg", fmt.Errorf("%s: %w", dsn, &pgconn.PgError{Code: "40001"}), "postgres_40001"},
		{"pg odd code", &pgconn.PgError{Code: "bad code with " + dsn}, "postgres"},
		{"plain", errors.New(dsn), "other"},
		{"typed nil pg", (*pgconn.PgError)(nil), "postgres"},
		{"wrapped typed nil pg", fmt.Errorf("x: %w", (*pgconn.PgError)(nil)), "postgres"},
		{"context wins over pg", fmt.Errorf("%w %w", &pgconn.PgError{Code: "40001"}, context.Canceled), "context_canceled"},
		{"canceled message wrapping", fmt.Errorf("query %s: %w", dsn, context.Canceled), "context_canceled"},
	}
	for _, tc := range cases {
		if got := Of(tc.err); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, got, tc.want)
		}
	}
}
