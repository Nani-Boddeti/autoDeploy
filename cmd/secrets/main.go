// Command secrets performs terminal-only, explicit KEK rotation operations.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"autodeploy/internal/config"
	secretcrypto "autodeploy/internal/crypto"
	"autodeploy/internal/secret"
	"autodeploy/internal/store/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/term"
)

type command struct {
	action, oldKEK, cursor string
	batch                  int
}

func parseArgs(args []string) (command, error) {
	if len(args) < 1 || args[0] != "secret" {
		return command{}, errors.New("invalid arguments")
	}
	if len(args) == 4 && args[1] == "can-retire" && args[2] == "--kek" && validID(args[3]) {
		return command{action: "can-retire", oldKEK: args[3]}, nil
	}
	if len(args) == 4 && args[1] == "init-active-kek" && args[2] == "--kek" && validID(args[3]) {
		return command{action: "init-active-kek", oldKEK: args[3]}, nil
	}
	if len(args) < 4 || args[1] != "rewrap" || args[2] != "--from-kek" || !validID(args[3]) {
		return command{}, errors.New("invalid arguments")
	}
	c := command{action: "rewrap", oldKEK: args[3], batch: secret.DefaultRewrapBatchSize}
	for i := 4; i < len(args); i += 2 {
		if i+1 >= len(args) {
			return command{}, errors.New("invalid arguments")
		}
		switch args[i] {
		case "--cursor":
			if !validID(args[i+1]) {
				return command{}, errors.New("invalid arguments")
			}
			c.cursor = args[i+1]
		case "--batch-size":
			n, e := strconv.Atoi(args[i+1])
			if e != nil || n < 1 || n > secret.MaxRewrapBatchSize {
				return command{}, errors.New("invalid arguments")
			}
			c.batch = n
		default:
			return command{}, errors.New("invalid arguments")
		}
	}
	return c, nil
}
func validID(v string) bool { return len(v) > 0 && len(v) <= 128 }
func main() {
	c, err := parseArgs(os.Args[1:])
	if err == nil {
		err = run(c, preflightTerminal, execute)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "secret operation failed")
		os.Exit(1)
	}
}

type terminalSession interface{ io.Closer }
type ttyTerminal struct{ file *os.File }

func preflightTerminal() (terminalSession, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, errors.New("interactive terminal required")
	}
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil || !term.IsTerminal(int(f.Fd())) {
		if f != nil {
			_ = f.Close()
		}
		return nil, errors.New("interactive terminal required")
	}
	return ttyTerminal{f}, nil
}
func (t ttyTerminal) Close() error { return t.file.Close() }
func run(c command, preflight func() (terminalSession, error), exec func(context.Context, command) (string, error)) error {
	return runWithOutput(c, preflight, exec, os.Stdout)
}
func runWithOutput(c command, preflight func() (terminalSession, error), exec func(context.Context, command) (string, error), output io.Writer) error {
	t, err := preflight()
	if err != nil {
		return err
	}
	defer t.Close()
	result, err := exec(context.Background(), c)
	if err == nil {
		_, _ = fmt.Fprintln(output, result)
	}
	return err
}
func execute(ctx context.Context, c command) (string, error) {
	return executeWith(ctx, c, productionDependencies())
}

type secretDependencies struct {
	loadDatabase func() (string, error)
	loadRing     func() (secretcrypto.KeyRing, string, error)
	open         func(context.Context, string) (secretOperator, error)
}
type secretOperator interface {
	io.Closer
	Initialize(context.Context, string, string) error
	CanRetire(context.Context, string, secretcrypto.KeyRing) error
	Rewrap(context.Context, secret.RewrapRequest, secretcrypto.KeyRing) (secret.RewrapResult, error)
}
type postgresSecretOperator struct{ pool *pgxpool.Pool }

func (o postgresSecretOperator) Close() error { o.pool.Close(); return nil }
func (o postgresSecretOperator) Initialize(ctx context.Context, id, generation string) error {
	return postgres.NewKeyRepository(o.pool).InitializeActiveLedger(ctx, id, generation)
}
func (o postgresSecretOperator) CanRetire(ctx context.Context, id string, ring secretcrypto.KeyRing) error {
	return secret.NewRotator(postgres.NewSecretRepository(o.pool), ring).CanRetire(ctx, id)
}
func (o postgresSecretOperator) Rewrap(ctx context.Context, request secret.RewrapRequest, ring secretcrypto.KeyRing) (secret.RewrapResult, error) {
	return secret.NewRotator(postgres.NewSecretRepository(o.pool), ring).RewrapBatch(ctx, request)
}

func productionDependencies() secretDependencies {
	return secretDependencies{loadDatabase: config.DatabaseURLFromEnvironment, loadRing: config.KEKRingAndGenerationFromEnvironment, open: func(ctx context.Context, dsn string) (secretOperator, error) {
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			return nil, err
		}
		return postgresSecretOperator{pool}, nil
	}}
}

func executeWith(ctx context.Context, c command, dependencies secretDependencies) (string, error) {
	if dependencies.loadDatabase == nil || dependencies.loadRing == nil || dependencies.open == nil {
		return "", errors.New("secret operation unavailable")
	}
	dsn, err := dependencies.loadDatabase()
	if err != nil {
		return "", err
	}
	ring, generation, err := dependencies.loadRing()
	if err != nil {
		return "", err
	}
	operator, err := dependencies.open(ctx, dsn)
	if err != nil {
		return "", err
	}
	defer operator.Close() //nolint:errcheck
	if c.action == "init-active-kek" {
		active, key, activeErr := ring.Active()
		for i := range key {
			key[i] = 0
		}
		if activeErr != nil || active != c.oldKEK {
			return "", errors.New("active KEK mismatch")
		}
		if err := operator.Initialize(ctx, c.oldKEK, generation); err != nil {
			return "", err
		}
		return "active secret key initialized", nil
	}
	if c.action == "can-retire" {
		if err := operator.CanRetire(ctx, c.oldKEK, ring); err != nil {
			return "", err
		}
		return "secret key can retire", nil
	}
	result, err := operator.Rewrap(ctx, secret.RewrapRequest{OldKEKID: c.oldKEK, Cursor: c.cursor, BatchSize: c.batch}, ring)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("secret rewrap completed selected=%d rewrapped=%d conflicts=%d cursor=%s", result.Selected, result.Rewrapped, result.Conflicts, result.NextCursor), nil
}
