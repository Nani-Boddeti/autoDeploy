package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"autodeploy/internal/config"
	"autodeploy/internal/errclass"
	"autodeploy/migrations"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// defaultTimeout bounds connect plus the whole migration run. Generous because
// a migration may rewrite a large table; the connection carries no
// lock/statement timeout of its own.
const defaultTimeout = 10 * time.Minute

// run returns the process exit code. Failures print a fixed message plus, for
// a database error, its SQLSTATE and constraint name only: driver error text
// can carry the DSN, row values or SQL detail.
func run(ctx context.Context, args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	timeout := flags.Duration("timeout", defaultTimeout, "overall time limit for connecting and applying migrations")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *timeout <= 0 {
		_, _ = fmt.Fprintln(stderr, "migrate: invalid arguments (usage: migrate [-timeout duration])")
		return 2
	}
	dsn, err := config.DatabaseURLFromEnvironment()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "migrate: load database credential: invalid configuration")
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "migrate: connect database failed class=%s\n", errclass.Of(err))
		return 1
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if err := migrations.Apply(ctx, conn); err != nil {
		_, _ = fmt.Fprintln(stderr, applyFailure(err))
		return 1
	}
	return 0
}

// applyFailure renders a migration failure from fixed text, the SQLSTATE class
// and the constraint name (a schema identifier, never row data).
func applyFailure(err error) string {
	msg := "migrate: apply migrations failed class=" + errclass.Of(err)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr != nil && pgErr.ConstraintName != "" {
		msg += " constraint=" + sanitizeIdent(pgErr.ConstraintName)
	}
	return msg
}

// sanitizeIdent keeps only identifier characters so a hostile name cannot
// inject log lines or terminal escapes.
func sanitizeIdent(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s) && len(b) < 63; i++ {
		c := s[i]
		if c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			b = append(b, c)
		}
	}
	return string(b)
}
