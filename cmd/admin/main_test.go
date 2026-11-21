package main

import "testing"

import (
	"autodeploy/internal/admincli"
	"errors"
	"io"
	"time"
)

func TestParseArgsRejectsEveryNonContractForm(t *testing.T) {
	operation, username, err := parseArgs([]string{"bootstrap", "--username", "Admin"})
	if err != nil || operation != "bootstrap" || username != "admin" {
		t.Fatalf("parse = %q %q %v", operation, username, err)
	}
	for _, args := range [][]string{
		nil,
		{"bootstrap", "--username"},
		{"bootstrap", "--password", "secret"},
		{"bootstrap", "--username", "admin", "--password", "secret"},
		{"bootstrap", "--username", "admin", "--username", "other"},
		{"unknown", "--username", "admin"},
	} {
		if _, _, err := parseArgs(args); err == nil {
			t.Fatalf("accepted %#v", args)
		}
	}
}

func TestTerminalRefusesPipedStdin(t *testing.T) {
	if _, err := (ttyTerminal{}).ReadPassword("Password: "); err == nil {
		t.Fatal("accepted non-terminal stdin")
	}
}

func TestRunPreflightsBeforeAnyCredentialOrDatabaseWork(t *testing.T) {
	events := []string{}
	terminal := &fakeSession{}
	err := run("bootstrap", "admin", func() (terminalSession, error) { events = append(events, "tty"); return terminal, nil }, func(admincli.Terminal) error { events = append(events, "database"); return nil })
	if err != nil || len(events) != 2 || events[0] != "tty" || events[1] != "database" || !terminal.closed {
		t.Fatalf("events=%v close=%v err=%v", events, terminal.closed, err)
	}
}

func TestRunStopsOnPreflightAndClosesOnExecuteFailure(t *testing.T) {
	called := false
	if err := run("bootstrap", "admin", func() (terminalSession, error) { return nil, errors.New("tty") }, func(admincli.Terminal) error { called = true; return nil }); err == nil || called {
		t.Fatalf("called=%v err=%v", called, err)
	}
	terminal := &fakeSession{}
	if err := run("bootstrap", "admin", func() (terminalSession, error) { return terminal, nil }, func(admincli.Terminal) error { return errors.New("execute") }); err == nil || !terminal.closed {
		t.Fatalf("closed=%v err=%v", terminal.closed, err)
	}
}

type fakeSession struct{ closed bool }

func (*fakeSession) ReadPassword(string) ([]byte, error) { return nil, errors.New("not used") }
func (s *fakeSession) Close() error                      { s.closed = true; return nil }

var _ io.Closer = (*fakeSession)(nil)

func TestSetupTokenCommandIsExactAndNotAPasswordOperation(t *testing.T) {
	if !isSetupTokenCommand([]string{"setup-token"}) {
		t.Fatal("setup-token not recognized")
	}
	for _, args := range [][]string{nil, {"setup-token", "--username", "a"}, {"setup-token", "x"}, {"Setup-Token"}, {"bootstrap", "--username", "admin"}} {
		if isSetupTokenCommand(args) {
			t.Fatalf("accepted %#v", args)
		}
	}
	// The password operations must keep rejecting it: it is not a parseArgs form.
	if _, _, err := parseArgs([]string{"setup-token"}); err == nil {
		t.Fatal("parseArgs accepted setup-token")
	}
}

func TestParseServerEnrollLocal(t *testing.T) {
	host := func() (string, error) { return "web1.example.com", nil }
	opts, err := parseServerEnrollLocal([]string{"--token-file", "/t", "--wait", "5m"}, host)
	if err != nil || opts.Name != "web1" || opts.TokenFile != "/t" || opts.Wait != 5*time.Minute {
		t.Fatalf("opts=%+v err=%v", opts, err)
	}
	opts, err = parseServerEnrollLocal([]string{"--token-file", "/t", "--name", "mine", "--server-id", "server_x"}, host)
	if err != nil || opts.Name != "mine" || opts.ServerID != "server_x" {
		t.Fatalf("opts=%+v err=%v", opts, err)
	}
	if opts, err = parseServerEnrollLocal([]string{"--token-file", "/t"}, func() (string, error) { return "", errors.New("x") }); err != nil || opts.Name != "local" {
		t.Fatalf("fallback name: %+v %v", opts, err)
	}
	for _, args := range [][]string{nil, {"--name", "x"}, {"--token-file", "/t", "extra"}, {"--token-file", "/t", "--wait", "-1s"}, {"--token-file", "/t", "--token", "enr.v1.x"}, {"--token-file", "/t", "--name", " bad"}} {
		if _, err := parseServerEnrollLocal(args, host); err == nil {
			t.Fatalf("accepted %#v", args)
		}
	}
}

func TestParseServerEnrollLocalWaitOnly(t *testing.T) {
	host := func() (string, error) { return "h", nil }
	opts, err := parseServerEnrollLocal([]string{"--wait-only", "--wait", "2m"}, host)
	if err != nil || !opts.WaitOnly || opts.TokenFile != "" || opts.Wait != 2*time.Minute {
		t.Fatalf("opts=%+v err=%v", opts, err)
	}
	for _, args := range [][]string{{"--wait-only", "--token-file", "/t"}, {"--wait-only", "--name", "x"}, {"--wait-only", "--server-id", "s"}} {
		if _, err := parseServerEnrollLocal(args, host); err == nil {
			t.Fatalf("accepted %#v", args)
		}
	}
}
