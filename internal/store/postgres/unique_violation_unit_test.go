package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsUniqueViolationOf(t *testing.T) {
	const name = "release_operations_environment_open_activation_idx"
	cases := []struct {
		label string
		err   error
		want  bool
	}{
		{"matching constraint", &pgconn.PgError{Code: "23505", ConstraintName: name}, true},
		{"wrapped matching", fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23505", ConstraintName: name}), true},
		// release_operations' primary key: the only other unique constraint on
		// the table (the integration test proves it against a real database).
		{"different unique constraint", &pgconn.PgError{Code: "23505", ConstraintName: "release_operations_pkey"}, false},
		{"empty constraint name", &pgconn.PgError{Code: "23505"}, false},
		{"same name, other SQLSTATE", &pgconn.PgError{Code: "23503", ConstraintName: name}, false},
		{"not a pg error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := isUniqueViolationOf(c.err, name); got != c.want {
			t.Errorf("%s: got %v want %v", c.label, got, c.want)
		}
	}
}
