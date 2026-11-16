package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"autodeploy/internal/secret"
	"autodeploy/internal/store/postgres"
)

// sentinel is a fake secret value; no output or error may ever contain it.
const sentinel = "SENTINEL-value-must-never-appear"

func promptsOf(values ...string) func(string) ([]byte, error) {
	i := 0
	return func(string) ([]byte, error) {
		if i >= len(values) {
			return nil, errors.New("unexpected prompt")
		}
		v := values[i]
		i++
		return []byte(v), nil
	}
}

func putCommand(prompt func(string) ([]byte, error)) command {
	return command{action: "put", environment: "uat", name: "NPM_TOKEN", scope: "build", prompt: prompt}
}

func TestParseArgsPutAndReplace(t *testing.T) {
	c, e := parseArgs([]string{"secret", "put", "--name", "NPM_TOKEN", "--environment", "uat", "--scope", "build"})
	if e != nil || c.action != "put" || c.environment != "uat" || c.name != "NPM_TOKEN" || c.scope != "build" {
		t.Fatalf("%+v %v", c, e)
	}
	c, e = parseArgs([]string{"secret", "replace", "--secret", "secret-1"})
	if e != nil || c.action != "replace" || c.secretID != "secret-1" || c.compromised {
		t.Fatalf("%+v %v", c, e)
	}
	c, e = parseArgs([]string{"secret", "replace", "--compromised", "--secret", "secret-1"})
	if e != nil || !c.compromised {
		t.Fatalf("%+v %v", c, e)
	}
	bad := [][]string{
		{"secret", "put"},
		{"secret", "put", "--environment", "uat", "--name", "NPM_TOKEN"},
		{"secret", "put", "--environment", "uat", "--name", "NPM_TOKEN", "--scope", "provider"},
		{"secret", "put", "--environment", "uat", "--name", "1BAD", "--scope", "build"},
		{"secret", "put", "--environment", "uat", "--name", "A-B", "--scope", "runtime"},
		{"secret", "put", "--environment", "uat", "--name", "A", "--scope", "runtime", "--value", "x"},
		{"secret", "put", "--environment", "uat", "--environment", "uat", "--name", "A", "--scope", "runtime"},
		{"secret", "put", "--environment", "", "--name", "A", "--scope", "runtime"},
		{"secret", "put", "--environment", "uat", "--name", "A", "--scope", "runtime", "--compromised"},
		{"secret", "replace"},
		{"secret", "replace", "--secret"},
		{"secret", "replace", "--secret", "x", "--secret", "y"},
		{"secret", "replace", "--secret", "x", "--value", "v"},
		{"secret", "replace", "--secret", "x", "--compromised", "--compromised"},
		{"secret", "replace", "--compromised"},
	}
	for _, a := range bad {
		if _, e := parseArgs(a); e == nil {
			t.Fatalf("accepted %v", a)
		}
	}
}

func TestPutDerivesScopeFromEnvironmentAndNeverPrintsValue(t *testing.T) {
	op := &fakeSecretOperator{envScope: secret.Scope{OwnerID: "owner-1", ProjectID: "project-1", EnvironmentID: "uat"}}
	deps := testSecretDeps(t, op)
	out, err := executeWith(context.Background(), putCommand(promptsOf(sentinel, sentinel)), deps)
	if err != nil {
		t.Fatal(err)
	}
	if out != "secret head id: secret-fixed\nversion: 1" {
		t.Fatalf("out=%q", out)
	}
	if strings.Contains(out, sentinel) {
		t.Fatal("output leaks value")
	}
	if len(op.created) != 1 || len(op.replaced) != 0 {
		t.Fatalf("calls=%v", op.calls)
	}
	got := op.created[0]
	want := secret.Scope{OwnerID: "owner-1", ProjectID: "project-1", EnvironmentID: "uat", SecretID: "secret-fixed", Purpose: "build", DeliveryScope: "build"}
	if got.scope != want || got.versionID != "secret-version-fixed" || got.plaintext != sentinel {
		t.Fatalf("created=%+v", got)
	}
	if got.op != (secret.OperationContext{AuditID: "secret-audit-fixed", ActorID: "terminal:tester"}) {
		t.Fatalf("op=%+v", got.op)
	}
}

func TestPutRefusals(t *testing.T) {
	long := strings.Repeat("x", 64<<10+1)
	cases := []struct {
		name    string
		prompt  func(string) ([]byte, error)
		envErr  error
		writeAt bool
	}{
		{"mismatch", promptsOf(sentinel, sentinel+"x"), nil, false},
		{"empty", promptsOf("", ""), nil, false},
		{"too long", promptsOf(long, long), nil, false},
		{"unknown environment", promptsOf(sentinel, sentinel), postgres.ErrEnvironmentNotFound, false},
		{"prompt failure", promptsOf(), nil, false},
	}
	for _, tc := range cases {
		op := &fakeSecretOperator{envScope: secret.Scope{OwnerID: "o", ProjectID: "p", EnvironmentID: "uat"}, envErr: tc.envErr}
		out, err := executeWith(context.Background(), putCommand(tc.prompt), testSecretDeps(t, op))
		if err == nil || out != "" || len(op.created) != 0 {
			t.Fatalf("%s: out=%q err=%v created=%d", tc.name, out, err, len(op.created))
		}
		if strings.Contains(err.Error(), sentinel) {
			t.Fatalf("%s: error leaks value", tc.name)
		}
	}
	// An unknown environment is reported before any prompt is shown.
	op := &fakeSecretOperator{envErr: postgres.ErrEnvironmentNotFound}
	prompted := false
	c := putCommand(func(string) ([]byte, error) { prompted = true; return nil, errors.New("x") })
	if _, err := executeWith(context.Background(), c, testSecretDeps(t, op)); err == nil || prompted {
		t.Fatalf("err=%v prompted=%v", err, prompted)
	}
	var ue userError
	if !errors.As(err0(executeWith(context.Background(), c, testSecretDeps(t, op))), &ue) {
		t.Fatal("unknown environment must be a user-facing error")
	}
}

func err0(_ string, err error) error { return err }

func TestPutServiceFailureIsGeneric(t *testing.T) {
	op := &fakeSecretOperator{envScope: secret.Scope{OwnerID: "o", ProjectID: "p", EnvironmentID: "uat"}, writeErr: errors.New("db detail " + sentinel)}
	out, err := executeWith(context.Background(), putCommand(promptsOf(sentinel, sentinel)), testSecretDeps(t, op))
	if err == nil || out != "" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if strings.Contains(err.Error(), sentinel) || err.Error() != "secret create failed" {
		t.Fatalf("create failure message not the fixed generic text: %d bytes", len(err.Error()))
	}
	if msg := errorMessage(err); strings.Contains(msg, sentinel) || msg != "secret operation failed" {
		t.Fatal("printed message leaks or is not generic")
	}
}

func TestReplaceServiceFailureIsGeneric(t *testing.T) {
	head := secret.Scope{OwnerID: "o", ProjectID: "p", EnvironmentID: "uat", SecretID: "secret-9", Purpose: "runtime", DeliveryScope: "runtime"}
	for _, compromised := range []bool{false, true} {
		op := &fakeSecretOperator{headScope: head, writeErr: errors.New("db detail " + sentinel)}
		var stdout bytes.Buffer
		c := command{action: "replace", secretID: "secret-9", compromised: compromised, prompt: promptsOf(sentinel, sentinel)}
		err := runWithOutput(c, func() (terminalSession, error) { return &promptTerminal{values: []string{sentinel, sentinel}}, nil }, func(ctx context.Context, c command) (string, error) {
			return executeWith(ctx, c, testSecretDeps(t, op))
		}, &stdout)
		if err == nil || strings.Contains(err.Error(), sentinel) || err.Error() != "secret replace failed" {
			t.Fatalf("compromised=%v: replace failure message not the fixed generic text", compromised)
		}
		if strings.Contains(stdout.String(), sentinel) || stdout.Len() != 0 || strings.Contains(errorMessage(err), sentinel) {
			t.Fatalf("compromised=%v: sentinel or output on failure path", compromised)
		}
	}
}

func TestAmbiguousEnvironmentIsFixedUserMessage(t *testing.T) {
	op := &fakeSecretOperator{envErr: fmt.Errorf("wrapped: %w", postgres.ErrEnvironmentAmbiguous)}
	_, err := executeWith(context.Background(), putCommand(promptsOf(sentinel, sentinel)), testSecretDeps(t, op))
	var ue userError
	if !errors.As(err, &ue) || errorMessage(err) != "secret operation failed: environment has more than one owner or project; refusing" {
		t.Fatalf("msg=%q", errorMessage(err))
	}
	if len(op.created) != 0 {
		t.Fatal("wrote despite ambiguity")
	}
}

func TestErrorMessageUsage(t *testing.T) {
	if errorMessage(errInvalidArguments) != usageText || errorMessage(fmt.Errorf("x: %w", errInvalidArguments)) != usageText {
		t.Fatal("usage errors must map to usage text")
	}
	if _, err := parseArgs([]string{"secret", "rewrap", "--from-kek", "old", "--batch-size", "0"}); !errors.Is(err, errInvalidArguments) {
		t.Fatal("rewrap parse error must be the usage sentinel")
	}
	if errorMessage(errors.New("invalid arguments")) == usageText {
		t.Fatal("string-equal foreign error must not be treated as usage")
	}
}

func TestReplacePaths(t *testing.T) {
	head := secret.Scope{OwnerID: "o", ProjectID: "p", EnvironmentID: "uat", SecretID: "secret-9", Purpose: "runtime", DeliveryScope: "runtime"}
	for _, compromised := range []bool{false, true} {
		op := &fakeSecretOperator{headScope: head}
		c := command{action: "replace", secretID: "secret-9", compromised: compromised, prompt: promptsOf(sentinel, sentinel)}
		out, err := executeWith(context.Background(), c, testSecretDeps(t, op))
		if err != nil || out != "secret head id: secret-9\nversion: 7" || strings.Contains(out, sentinel) {
			t.Fatalf("out=%q err=%v", out, err)
		}
		if len(op.replaced) != 1 || len(op.created) != 0 || op.replaced[0].compromised != compromised || op.replaced[0].scope != head || op.replaced[0].plaintext != sentinel {
			t.Fatalf("replaced=%+v", op.replaced)
		}
		if op.replaced[0].op != (secret.OperationContext{AuditID: "secret-audit-fixed", ActorID: "terminal:tester"}) || op.replaced[0].versionID != "secret-version-fixed" {
			t.Fatalf("op=%+v", op.replaced[0])
		}
	}
	// Unknown head and mismatched confirmation write nothing.
	op := &fakeSecretOperator{headErr: postgres.ErrSecretNotFound}
	c := command{action: "replace", secretID: "nope", prompt: promptsOf(sentinel, sentinel)}
	if out, err := executeWith(context.Background(), c, testSecretDeps(t, op)); err == nil || out != "" || len(op.replaced) != 0 {
		t.Fatalf("out=%q err=%v", out, err)
	}
	op = &fakeSecretOperator{headScope: head}
	c = command{action: "replace", secretID: "secret-9", prompt: promptsOf("a", "b")}
	if _, err := executeWith(context.Background(), c, testSecretDeps(t, op)); err == nil || len(op.replaced) != 0 {
		t.Fatalf("err=%v", err)
	}
}

func TestNonTerminalRefusedBeforeAnyDatabaseAccess(t *testing.T) {
	var out bytes.Buffer
	c := putCommand(nil)
	err := runWithOutput(c, func() (terminalSession, error) { return nil, errors.New("interactive terminal required") },
		func(context.Context, command) (string, error) { t.Fatal("executed"); return "", nil }, &out)
	if err == nil || out.Len() != 0 {
		t.Fatalf("err=%v out=%q", err, out.String())
	}
}

func TestRunWiresTerminalPromptIntoCommand(t *testing.T) {
	var out bytes.Buffer
	term := promptTerminal{values: []string{"one", "two"}}
	c := command{action: "put"}
	err := runWithOutput(c, func() (terminalSession, error) { return &term, nil }, func(_ context.Context, c command) (string, error) {
		a, _ := c.prompt("Secret value: ")
		b, _ := c.prompt("Repeat value: ")
		return string(a) + string(b), nil
	}, &out)
	if err != nil || out.String() != "onetwo\n" || strings.Join(term.prompts, "|") != "Secret value: |Repeat value: " {
		t.Fatalf("err=%v out=%q prompts=%v", err, out.String(), term.prompts)
	}
}

type promptTerminal struct {
	values  []string
	prompts []string
}

func (p *promptTerminal) Close() error { return nil }
func (p *promptTerminal) ReadSecret(prompt string) ([]byte, error) {
	p.prompts = append(p.prompts, prompt)
	v := p.values[0]
	p.values = p.values[1:]
	return []byte(v), nil
}
