// Command admin manages the single AutoDeploy administrator through a local
// terminal. It intentionally has no non-interactive password input path.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"autodeploy/internal/admincli"
	"autodeploy/internal/auth"
	"autodeploy/internal/config"
	"autodeploy/internal/store/postgres"
	"autodeploy/migrations"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/term"
)

func main() {
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
		conn, connectErr := pgx.Connect(ctx, dsn)
		if connectErr != nil {
			return connectErr
		}
		if migrationErr := migrations.Apply(ctx, conn); migrationErr != nil {
			_ = conn.Close(ctx)
			return migrationErr
		}
		if closeErr := conn.Close(ctx); closeErr != nil {
			return closeErr
		}
		pool, poolErr := pgxpool.New(ctx, dsn)
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
	if err != nil {
		fail()
	}
	fmt.Fprintln(os.Stdout, "administrator operation completed")
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
