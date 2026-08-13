package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	secretcrypto "autodeploy/internal/crypto"
	"autodeploy/internal/secret"
)

func TestParseArgs(t *testing.T) {
	c, e := parseArgs([]string{"secret", "rewrap", "--from-kek", "old", "--cursor", "v1", "--batch-size", "100"})
	if e != nil || c.oldKEK != "old" || c.cursor != "v1" || c.batch != 100 {
		t.Fatalf("%+v %v", c, e)
	}
	c, e = parseArgs([]string{"secret", "init-active-kek", "--kek", "fresh"})
	if e != nil || c.action != "init-active-kek" || c.oldKEK != "fresh" {
		t.Fatalf("%+v %v", c, e)
	}
	for _, a := range [][]string{{"secret", "rewrap", "--from-kek", "old", "--batch-size", "101"}, {"secret", "can-retire", "--kek"}, {"secret", "rewrap", "--from-kek", "old", "--value", "x"}} {
		if _, e := parseArgs(a); e == nil {
			t.Fatalf("accepted %v", a)
		}
	}
}
func TestRunPreflightsBeforeExecute(t *testing.T) {
	events := []string{}
	err := run(command{}, func() (terminalSession, error) { events = append(events, "tty"); return fakeTerminal{}, nil }, func(_ context.Context, _ command) (string, error) {
		events = append(events, "execute")
		return "fixed", nil
	})
	if err != nil || len(events) != 2 || events[0] != "tty" {
		t.Fatalf("%v %v", events, err)
	}
	if err := run(command{}, func() (terminalSession, error) { return nil, errors.New("no tty") }, func(context.Context, command) (string, error) { t.Fatal("executed"); return "", nil }); err == nil {
		t.Fatal("preflight accepted")
	}
}
func TestRunWithOutputNeverWritesFailureDetails(t *testing.T) {
	var output bytes.Buffer
	err := runWithOutput(command{}, func() (terminalSession, error) { return fakeTerminal{}, nil }, func(context.Context, command) (string, error) {
		return "secret-bearing-error", errors.New("secret-bearing-error")
	}, &output)
	if err == nil || output.Len() != 0 {
		t.Fatalf("failure output=%q err=%v", output.String(), err)
	}
}

type fakeTerminal struct{}

func (fakeTerminal) Close() error { return nil }

type fakeSecretOperator struct {
	init, retire error
	result       secret.RewrapResult
	rewrap       error
	calls        []string
}

func (f *fakeSecretOperator) Close() error { return nil }
func (f *fakeSecretOperator) Initialize(context.Context, string, string) error {
	f.calls = append(f.calls, "init")
	return f.init
}
func (f *fakeSecretOperator) CanRetire(context.Context, string, secretcrypto.KeyRing) error {
	f.calls = append(f.calls, "retire")
	return f.retire
}
func (f *fakeSecretOperator) Rewrap(_ context.Context, _ secret.RewrapRequest, _ secretcrypto.KeyRing) (secret.RewrapResult, error) {
	f.calls = append(f.calls, "rewrap")
	return f.result, f.rewrap
}
func testSecretDeps(t *testing.T, operator *fakeSecretOperator) secretDependencies {
	t.Helper()
	ring, err := secretcrypto.NewKeyRing("fresh", map[string][]byte{"fresh": []byte("01234567890123456789012345678901")})
	if err != nil {
		t.Fatal(err)
	}
	return secretDependencies{loadDatabase: func() (string, error) { return "database", nil }, loadRing: func() (secretcrypto.KeyRing, string, error) { return ring, "0123456789abcdef", nil }, open: func(context.Context, string) (secretOperator, error) { return operator, nil }}
}
func TestExecuteWithSanitizesOperationErrorsAndFormatsResults(t *testing.T) {
	operator := &fakeSecretOperator{}
	deps := testSecretDeps(t, operator)
	got, err := executeWith(context.Background(), command{action: "init-active-kek", oldKEK: "fresh"}, deps)
	if err != nil || got != "active secret key initialized" || len(operator.calls) != 1 || operator.calls[0] != "init" {
		t.Fatalf("init=%q %v %#v", got, err, operator.calls)
	}
	operator.init = errors.New("secret-bearing-error")
	if got, err = executeWith(context.Background(), command{action: "init-active-kek", oldKEK: "fresh"}, deps); err == nil || got != "" {
		t.Fatalf("duplicate=%q %v", got, err)
	}
	operator.init = nil
	operator.result = secret.RewrapResult{Selected: 2, Rewrapped: 1, Conflicts: 1, NextCursor: "cursor"}
	got, err = executeWith(context.Background(), command{action: "rewrap", oldKEK: "old", batch: 100}, deps)
	if err != nil || got != "secret rewrap completed selected=2 rewrapped=1 conflicts=1 cursor=cursor" {
		t.Fatalf("rewrap=%q %v", got, err)
	}
	operator.retire = errors.New("active secret-bearing-error")
	if got, err = executeWith(context.Background(), command{action: "can-retire", oldKEK: "fresh"}, deps); err == nil || got != "" {
		t.Fatalf("retire=%q %v", got, err)
	}
}
