// Command secrets performs terminal-only secret operations: KEK rotation
// (init-active-kek, rewrap, can-retire) and secret value entry (put, replace).
package main

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"

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
	// put/replace: the value is only ever read through prompt, never argv.
	environment, name, scope, secretID string
	compromised                        bool
	prompt                             func(string) ([]byte, error)
}

const usageText = "usage: secret put --environment ENV --name VAR --scope runtime|build | secret replace --secret ID [--compromised]"

// userError carries a fixed, value-free message that is safe to show.
type userError string

func (e userError) Error() string { return string(e) }

// errInvalidArguments marks a usage error; match it with errors.Is.
var errInvalidArguments = errors.New("invalid arguments")

// parseWriteArgs parses the flags of put/replace. Each value flag must appear
// exactly once; --compromised is a boolean accepted only when allowed.
func parseWriteArgs(args []string, valueFlags []string, allowCompromised bool) (map[string]string, bool, error) {
	seen := map[string]string{}
	compromised := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--compromised" && allowCompromised && !compromised {
			compromised = true
			continue
		}
		known := false
		for _, f := range valueFlags {
			known = known || f == a
		}
		if !known || i+1 >= len(args) || args[i+1] == "" {
			return nil, false, errInvalidArguments
		}
		if _, dup := seen[a]; dup {
			return nil, false, errInvalidArguments
		}
		seen[a] = args[i+1]
		i++
	}
	if len(seen) != len(valueFlags) {
		return nil, false, errInvalidArguments
	}
	return seen, compromised, nil
}

func parseArgs(args []string) (command, error) {
	if len(args) < 1 || args[0] != "secret" {
		return command{}, errInvalidArguments
	}
	if len(args) >= 2 && args[1] == "put" {
		f, _, err := parseWriteArgs(args[2:], []string{"--environment", "--name", "--scope"}, false)
		if err != nil || !validID(f["--environment"]) || !postgres.ValidVariableName(f["--name"]) || (f["--scope"] != "runtime" && f["--scope"] != "build") {
			return command{}, errInvalidArguments
		}
		return command{action: "put", environment: f["--environment"], name: f["--name"], scope: f["--scope"]}, nil
	}
	if len(args) >= 2 && args[1] == "replace" {
		f, compromised, err := parseWriteArgs(args[2:], []string{"--secret"}, true)
		if err != nil || !validID(f["--secret"]) {
			return command{}, errInvalidArguments
		}
		return command{action: "replace", secretID: f["--secret"], compromised: compromised}, nil
	}
	if len(args) == 4 && args[1] == "can-retire" && args[2] == "--kek" && validID(args[3]) {
		return command{action: "can-retire", oldKEK: args[3]}, nil
	}
	if len(args) == 4 && args[1] == "init-active-kek" && args[2] == "--kek" && validID(args[3]) {
		return command{action: "init-active-kek", oldKEK: args[3]}, nil
	}
	if len(args) < 4 || args[1] != "rewrap" || args[2] != "--from-kek" || !validID(args[3]) {
		return command{}, errInvalidArguments
	}
	c := command{action: "rewrap", oldKEK: args[3], batch: secret.DefaultRewrapBatchSize}
	for i := 4; i < len(args); i += 2 {
		if i+1 >= len(args) {
			return command{}, errInvalidArguments
		}
		switch args[i] {
		case "--cursor":
			if !validID(args[i+1]) {
				return command{}, errInvalidArguments
			}
			c.cursor = args[i+1]
		case "--batch-size":
			n, e := strconv.Atoi(args[i+1])
			if e != nil || n < 1 || n > secret.MaxRewrapBatchSize {
				return command{}, errInvalidArguments
			}
			c.batch = n
		default:
			return command{}, errInvalidArguments
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
		fmt.Fprintln(os.Stderr, errorMessage(err))
		os.Exit(1)
	}
}

// errorMessage maps an error to the fixed, value-free text shown to the operator.
func errorMessage(err error) string {
	var ue userError
	switch {
	case errors.As(err, &ue):
		return "secret operation failed: " + ue.Error()
	case errors.Is(err, errInvalidArguments):
		return usageText
	default:
		return "secret operation failed"
	}
}

// terminalSession is the controlling terminal; ReadSecret shows prompt and
// reads one line without echo.
type terminalSession interface {
	io.Closer
	ReadSecret(prompt string) ([]byte, error)
}
type ttyTerminal struct {
	file *os.File
	ops  ttyOps
}

// ttyOps are the terminal and signal primitives, injectable for tests.
type ttyOps struct {
	getState     func(fd int) (*term.State, error)
	restore      func(fd int, state *term.State) error
	readPassword func(fd int) ([]byte, error)
	notify       func(chan<- os.Signal)
	stop         func(chan<- os.Signal)
	exit         func(int)
}

func realTTYOps() ttyOps {
	return ttyOps{
		getState:     term.GetState,
		restore:      term.Restore,
		readPassword: term.ReadPassword,
		notify:       func(c chan<- os.Signal) { signal.Notify(c, os.Interrupt, syscall.SIGTERM) },
		stop:         signal.Stop,
		exit:         os.Exit,
	}
}

func preflightTerminal() (terminalSession, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, userError("interactive terminal required")
	}
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil || !term.IsTerminal(int(f.Fd())) {
		if f != nil {
			_ = f.Close()
		}
		return nil, userError("interactive terminal required")
	}
	return ttyTerminal{file: f, ops: realTTYOps()}, nil
}
func (t ttyTerminal) Close() error { return t.file.Close() }
func (t ttyTerminal) ReadSecret(prompt string) ([]byte, error) {
	return guardedReadSecret(int(t.file.Fd()), t.file, prompt, t.ops)
}

// guardedReadSecret reads one hidden line. The terminal state saved before the
// prompt is restored on every return path and, on SIGINT/SIGTERM, before the
// process exits non-zero without writing anything.
func guardedReadSecret(fd int, out io.Writer, prompt string, ops ttyOps) ([]byte, error) {
	state, err := ops.getState(fd)
	if err != nil {
		return nil, errors.New("terminal state unavailable")
	}
	if _, err := fmt.Fprint(out, prompt); err != nil {
		return nil, err
	}
	signals := make(chan os.Signal, 1)
	done := make(chan struct{})
	ops.notify(signals)
	go func() {
		select {
		case <-signals:
			_ = ops.restore(fd, state)
			ops.exit(130)
		case <-done:
		}
	}()
	defer func() {
		ops.stop(signals)
		close(done)
		_ = ops.restore(fd, state)
	}()
	v, err := ops.readPassword(fd)
	_, _ = fmt.Fprintln(out)
	return v, err
}
func run(c command, preflight func() (terminalSession, error), exec func(context.Context, command) (string, error)) error {
	return runWithOutput(c, preflight, exec, os.Stdout)
}
func runWithOutput(c command, preflight func() (terminalSession, error), exec func(context.Context, command) (string, error), output io.Writer) error {
	t, err := preflight()
	if err != nil {
		return err
	}
	defer t.Close()
	c.prompt = t.ReadSecret
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
	actor        func() string
	newID        func(prefix string) (string, error)
}
type secretOperator interface {
	io.Closer
	Initialize(context.Context, string, string) error
	CanRetire(context.Context, string, secretcrypto.KeyRing) error
	Rewrap(context.Context, secret.RewrapRequest, secretcrypto.KeyRing) (secret.RewrapResult, error)
	// EnvironmentScope returns a Scope holding only the environment's derived owner/project/environment ids.
	EnvironmentScope(context.Context, string) (secret.Scope, error)
	HeadScope(context.Context, string) (secret.Scope, error)
	Create(context.Context, secretcrypto.KeyRing, secret.Scope, string, secret.OperationContext, []byte) (secret.Metadata, error)
	Replace(context.Context, secretcrypto.KeyRing, secret.Scope, string, secret.OperationContext, []byte, bool) (secret.Metadata, error)
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

func (o postgresSecretOperator) EnvironmentScope(ctx context.Context, environmentID string) (secret.Scope, error) {
	owner, project, err := postgres.NewSecretRepository(o.pool).EnvironmentOwnerProject(ctx, environmentID)
	return secret.Scope{OwnerID: owner, ProjectID: project, EnvironmentID: environmentID}, err
}
func (o postgresSecretOperator) HeadScope(ctx context.Context, secretID string) (secret.Scope, error) {
	return postgres.NewSecretRepository(o.pool).SecretHeadScope(ctx, secretID)
}

// writeService needs no authorizer: only materialization consults one.
func (o postgresSecretOperator) writeService(ring secretcrypto.KeyRing) *secret.Service {
	return secret.NewService(postgres.NewSecretRepository(o.pool), ring, nil)
}
func (o postgresSecretOperator) Create(ctx context.Context, ring secretcrypto.KeyRing, scope secret.Scope, versionID string, op secret.OperationContext, plaintext []byte) (secret.Metadata, error) {
	return o.writeService(ring).CreateWithContext(ctx, scope, versionID, op, plaintext)
}
func (o postgresSecretOperator) Replace(ctx context.Context, ring secretcrypto.KeyRing, scope secret.Scope, versionID string, op secret.OperationContext, plaintext []byte, compromised bool) (secret.Metadata, error) {
	if compromised {
		return o.writeService(ring).ReplaceCompromisedWithContext(ctx, scope, versionID, op, plaintext)
	}
	return o.writeService(ring).ReplaceWithContext(ctx, scope, versionID, op, plaintext)
}

func generateID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := cryptorand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(raw[:]), nil
}

// terminalActor names the invoking unix account for the audit trail. Under
// autodeploy-run this is the control-plane service account.
func terminalActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return "terminal:" + u.Username
	}
	return "terminal:uid-" + strconv.Itoa(os.Getuid())
}

// readConfirmedValue prompts twice and returns the value; the caller clears it.
func readConfirmedValue(prompt func(string) ([]byte, error)) ([]byte, error) {
	if prompt == nil {
		return nil, errors.New("prompt unavailable")
	}
	first, err := prompt("Secret value: ")
	if err != nil {
		clear(first)
		return nil, errors.New("prompt failed")
	}
	second, err := prompt("Repeat value: ")
	defer clear(second)
	if err != nil {
		clear(first)
		return nil, errors.New("prompt failed")
	}
	switch {
	case len(first) == 0:
		clear(first)
		return nil, userError("value must not be empty")
	case len(first) > secretcrypto.MaxPlaintextBytes:
		clear(first)
		return nil, userError("value too long")
	case subtle.ConstantTimeCompare(first, second) != 1:
		clear(first)
		return nil, userError("values do not match")
	}
	return first, nil
}

func newOperation(dependencies secretDependencies) (secret.OperationContext, string, error) {
	auditID, err := dependencies.newID("secret-audit")
	if err != nil {
		return secret.OperationContext{}, "", errors.New("id generation failed")
	}
	versionID, err := dependencies.newID("secret-version")
	if err != nil {
		return secret.OperationContext{}, "", errors.New("id generation failed")
	}
	return secret.OperationContext{AuditID: auditID, ActorID: dependencies.actor()}, versionID, nil
}

func putSecret(ctx context.Context, c command, ring secretcrypto.KeyRing, operator secretOperator, dependencies secretDependencies) (string, error) {
	scope, err := operator.EnvironmentScope(ctx, c.environment)
	if errors.Is(err, postgres.ErrEnvironmentNotFound) {
		return "", userError("unknown environment")
	}
	if errors.Is(err, postgres.ErrEnvironmentAmbiguous) {
		return "", userError("environment has more than one owner or project; refusing")
	}
	if err != nil {
		return "", err
	}
	scope.Purpose, scope.DeliveryScope = c.scope, c.scope
	if scope.SecretID, err = dependencies.newID("secret"); err != nil {
		return "", errors.New("id generation failed")
	}
	value, err := readConfirmedValue(c.prompt)
	if err != nil {
		return "", err
	}
	defer clear(value)
	op, versionID, err := newOperation(dependencies)
	if err != nil {
		return "", err
	}
	meta, err := operator.Create(ctx, ring, scope, versionID, op, value)
	if err != nil {
		return "", errors.New("secret create failed")
	}
	return fmt.Sprintf("secret head id: %s\nversion: %d", scope.SecretID, meta.Number), nil
}

func replaceSecret(ctx context.Context, c command, ring secretcrypto.KeyRing, operator secretOperator, dependencies secretDependencies) (string, error) {
	scope, err := operator.HeadScope(ctx, c.secretID)
	if errors.Is(err, postgres.ErrSecretNotFound) {
		return "", userError("unknown secret")
	}
	if err != nil {
		return "", err
	}
	value, err := readConfirmedValue(c.prompt)
	if err != nil {
		return "", err
	}
	defer clear(value)
	op, versionID, err := newOperation(dependencies)
	if err != nil {
		return "", err
	}
	meta, err := operator.Replace(ctx, ring, scope, versionID, op, value, c.compromised)
	if err != nil {
		return "", errors.New("secret replace failed")
	}
	return fmt.Sprintf("secret head id: %s\nversion: %d", scope.SecretID, meta.Number), nil
}

func productionDependencies() secretDependencies {
	return secretDependencies{actor: terminalActor, newID: generateID, loadDatabase: config.DatabaseURLFromEnvironment, loadRing: config.KEKRingAndGenerationFromEnvironment, open: func(ctx context.Context, dsn string) (secretOperator, error) {
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			return nil, err
		}
		return postgresSecretOperator{pool}, nil
	}}
}

func executeWith(ctx context.Context, c command, dependencies secretDependencies) (string, error) {
	if dependencies.loadDatabase == nil || dependencies.loadRing == nil || dependencies.open == nil || dependencies.actor == nil || dependencies.newID == nil {
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
	switch c.action {
	case "put":
		return putSecret(ctx, c, ring, operator, dependencies)
	case "replace":
		return replaceSecret(ctx, c, ring, operator, dependencies)
	}
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
