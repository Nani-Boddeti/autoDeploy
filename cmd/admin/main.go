// Command admin manages the single AutoDeploy administrator through a local
// terminal. It intentionally has no non-interactive password input path.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"autodeploy/internal/admincli"
	"autodeploy/internal/auth"
	"autodeploy/internal/config"
	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
	"autodeploy/migrations"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/term"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == serverEnrollLocalCommand {
		runServerEnrollLocal(os.Args[2:])
		return
	}
	if isSetupTokenCommand(os.Args[1:]) {
		runSetupToken()
		return
	}
	operation, username, err := parseArgs(os.Args[1:])
	if err != nil {
		fail()
	}
	err = run(operation, username, preflightTerminal, func(terminal admincli.Terminal) error {
		dsn, configErr := config.DatabaseURLFromEnvironment()
		if configErr != nil {
			return configErr
		}
		ctx := context.Background()
		pool, poolErr := openPool(ctx, dsn)
		if poolErr != nil {
			return poolErr
		}
		defer pool.Close()
		dependencies := admincli.Dependencies{Repository: postgres.NewAuthRepository(pool), Terminal: terminal, Benchmark: benchmarkArgon}
		if operation == "bootstrap" {
			return admincli.Bootstrap(ctx, username, dependencies)
		}
		ring, ringErr := config.UsernameThrottleKeyRingFromEnvironment()
		if ringErr != nil {
			return ringErr
		}
		return admincli.ResetPassword(ctx, username, ring, dependencies)
	})
	if errors.Is(err, errSchemaNotCurrent) {
		fmt.Fprintln(os.Stderr, errSchemaNotCurrent.Error())
		os.Exit(1)
	}
	if err != nil {
		fail()
	}
	fmt.Fprintln(os.Stdout, "administrator operation completed")
}

// isSetupTokenCommand reports the exact argument form `setup-token` (no flags,
// no value). It is a separate path from the terminal-only password commands:
// it never reads a password and needs no TTY, so the installer can capture
// the printed code.
func isSetupTokenCommand(args []string) bool { return len(args) == 1 && args[0] == "setup-token" }

// runSetupToken mints a browser first-run code. Only the code goes to stdout.
func runSetupToken() {
	err := func() error {
		dsn, configErr := config.DatabaseURLFromEnvironment()
		if configErr != nil {
			return configErr
		}
		ctx := context.Background()
		pool, poolErr := openPool(ctx, dsn)
		if poolErr != nil {
			return poolErr
		}
		defer pool.Close()
		return admincli.SetupToken(ctx, postgres.NewAuthRepository(pool), nil, os.Stdout)
	}()
	switch {
	case errors.Is(err, errSchemaNotCurrent):
		fmt.Fprintln(os.Stderr, errSchemaNotCurrent.Error())
		os.Exit(1)
	case errors.Is(err, admincli.ErrAdministratorExists):
		fmt.Fprintln(os.Stderr, "an administrator already exists; first-run setup is closed")
		os.Exit(1)
	case err != nil:
		fail()
	}
	fmt.Fprintf(os.Stderr, "setup code valid for %d minutes; open /setup and enter it. Older unused codes are now invalid.\n", int(admincli.SetupTokenTTL.Minutes()))
}

const serverEnrollLocalCommand = "server-enroll-local"

// parseServerEnrollLocal parses `server-enroll-local --token-file PATH [--name N]
// [--server-id ID] [--wait DURATION]`. There is deliberately no flag that takes
// a token: the token only ever goes to the file.
func parseServerEnrollLocal(args []string, hostname func() (string, error)) (admincli.LocalEnrollOptions, error) {
	var opts admincli.LocalEnrollOptions
	fs := flag.NewFlagSet(serverEnrollLocalCommand, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.TokenFile, "token-file", "", "")
	fs.StringVar(&opts.Name, "name", "", "")
	fs.StringVar(&opts.ServerID, "server-id", "", "")
	fs.DurationVar(&opts.Wait, "wait", 0, "")
	fs.BoolVar(&opts.WaitOnly, "wait-only", false, "")
	// --wait-only takes no token file: it only waits for/checks the administrator.
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || (opts.TokenFile == "") == !opts.WaitOnly || opts.Wait < 0 || (opts.WaitOnly && (opts.Name != "" || opts.ServerID != "")) {
		return opts, errors.New("usage: admin server-enroll-local --token-file PATH [--name NAME] [--server-id ID] [--wait DURATION] | --wait-only [--wait DURATION]")
	}
	if opts.WaitOnly {
		return opts, nil
	}
	if opts.Name == "" {
		host, err := hostname()
		if err != nil {
			host = ""
		}
		opts.Name, _, _ = strings.Cut(strings.TrimSpace(host), ".")
		if opts.Name == "" {
			opts.Name = "local"
		}
	}
	if !server.ValidName(opts.Name) {
		return opts, errors.New("invalid --name")
	}
	return opts, nil
}

// Exit codes of server-enroll-local (stdout carries only the server id). They avoid 2 (Go panic)
// and the shell's 126+ range; any other nonzero code is a generic failure.
const (
	exitNoAdministrator  = 20 // first-run setup pending
	exitNeedsReenroll    = 21 // token consumed, no active credential
	exitRecordedIDGone   = 22 // recorded server id revoked or removed
	exitAdministratorOff = 23 // administrator account disabled
	exitAlreadyEnrolled  = 30 // active credential exists: id printed, nothing minted
)

// runServerEnrollLocal registers this host's server and writes its enrollment
// token to --token-file. stdout carries only the server id; the token never
// reaches stdout, stderr or any log. It needs no TTY and no password.
// --wait-only just waits for an administrator (no token, no file).
func runServerEnrollLocal(args []string) {
	opts, err := parseServerEnrollLocal(args, os.Hostname)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	alreadyEnrolled := false
	err = func() error {
		dsn, configErr := config.DatabaseURLFromEnvironment()
		if configErr != nil {
			return configErr
		}
		ctx := context.Background()
		pool, poolErr := openPool(ctx, dsn)
		if poolErr != nil {
			return poolErr
		}
		defer pool.Close()
		repo := postgres.NewServerRepository(pool)
		if opts.WaitOnly {
			_, waitErr := admincli.WaitForAdministrator(ctx, repo, opts.Wait, nil)
			return waitErr
		}
		var enrollErr error
		alreadyEnrolled, enrollErr = admincli.EnrollLocal(ctx, repo, opts, os.Stdout)
		return enrollErr
	}()
	codes := []struct {
		target error
		code   int
	}{
		{admincli.ErrAdministratorMissing, exitNoAdministrator},
		{admincli.ErrServerNeedsReenroll, exitNeedsReenroll},
		{admincli.ErrLocalServerUnavailable, exitRecordedIDGone},
		{admincli.ErrAdministratorDisabled, exitAdministratorOff},
		{admincli.ErrTokenFileUnusable, 1},
	}
	switch {
	case err == nil:
		if alreadyEnrolled {
			os.Exit(exitAlreadyEnrolled)
		}
		return
	case errors.Is(err, errSchemaNotCurrent):
		fmt.Fprintln(os.Stderr, errSchemaNotCurrent.Error())
		os.Exit(1)
	}
	for _, c := range codes {
		if errors.Is(err, c.target) {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(c.code)
		}
	}
	fmt.Fprintln(os.Stderr, "server enrollment failed")
	os.Exit(1)
}

// errSchemaNotCurrent is the only database-readiness text shown to the operator.
var errSchemaNotCurrent = errors.New("database schema is not current; run autodeploy-run migrate")

// openPool never migrates: admin runs as the runtime role (no CREATE, no
// ledger writes). It verifies read-only that every embedded migration is
// recorded, then returns the pool.
func openPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	checkErr := migrations.CheckCurrent(ctx, conn)
	if closeErr := conn.Close(ctx); checkErr == nil {
		checkErr = closeErr
	}
	if errors.Is(checkErr, migrations.ErrNotCurrent) {
		return nil, errSchemaNotCurrent
	}
	if checkErr != nil {
		return nil, checkErr
	}
	return pgxpool.New(ctx, dsn)
}

func parseArgs(args []string) (string, string, error) {
	if len(args) != 3 || (args[0] != "bootstrap" && args[0] != "reset-password") || args[1] != "--username" {
		return "", "", errors.New("invalid arguments")
	}
	username, err := auth.CanonicalUsername(args[2])
	if err != nil {
		return "", "", errors.New("invalid arguments")
	}
	return args[0], username, nil
}

type terminalSession interface {
	admincli.Terminal
	io.Closer
}

func run(operation, username string, preflight func() (terminalSession, error), execute func(admincli.Terminal) error) error {
	terminal, err := preflight()
	if err != nil {
		return err
	}
	defer terminal.Close() //nolint:errcheck
	return execute(terminal)
}

type ttyTerminal struct{ file *os.File }

func preflightTerminal() (terminalSession, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, errors.New("interactive terminal required")
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, errors.New("interactive terminal required")
	}
	if !term.IsTerminal(int(tty.Fd())) {
		_ = tty.Close()
		return nil, errors.New("interactive terminal required")
	}
	return ttyTerminal{file: tty}, nil
}

func (t ttyTerminal) ReadPassword(prompt string) ([]byte, error) {
	if t.file == nil {
		return nil, errors.New("interactive terminal required")
	}
	if _, err := io.WriteString(t.file, prompt); err != nil {
		return nil, errors.New("terminal write failed")
	}
	value, err := term.ReadPassword(int(t.file.Fd()))
	if _, writeErr := io.WriteString(t.file, "\n"); err == nil && writeErr != nil {
		err = writeErr
	}
	if err != nil {
		return nil, errors.New("terminal read failed")
	}
	return value, nil
}
func (t ttyTerminal) Close() error {
	if t.file == nil {
		return nil
	}
	return t.file.Close()
}

func benchmarkArgon(policy auth.Argon2Policy) (time.Duration, error) {
	start := time.Now()
	_, err := auth.HashPassword("autodeploy-calibration-password", policy, bytes.NewReader(bytes.Repeat([]byte{0x6d}, 16)))
	return time.Since(start), err
}

func fail() {
	fmt.Fprintln(os.Stderr, "administrator operation failed")
	os.Exit(1)
}
