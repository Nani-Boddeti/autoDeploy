// Package errclass reduces an error to a fixed, secret-free token for process
// logs. Error text can carry a DSN, token or SQL detail, so logs record the
// failing step plus this class instead of the text.
package errclass

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// Of returns one of: none, context_canceled, deadline_exceeded,
// postgres_<SQLSTATE>, postgres, other. Only fixed tokens and a validated
// 5-character SQLSTATE are ever returned, never any part of the error text.
func Of(err error) string {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.As(err, &pgErr):
		// A typed-nil *PgError satisfies As; it must not be dereferenced.
		if pgErr != nil && validSQLState(pgErr.Code) {
			return "postgres_" + pgErr.Code
		}
		return "postgres"
	default:
		return "other"
	}
}

func validSQLState(code string) bool {
	if len(code) != 5 {
		return false
	}
	for _, c := range code {
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}
