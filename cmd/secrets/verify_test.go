package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"autodeploy/internal/secret"
)

func TestParseArgsVerify(t *testing.T) {
	c, err := parseArgs([]string{"secret", "verify"})
	if err != nil || c.action != "verify" {
		t.Fatalf("%+v %v", c, err)
	}
	for _, a := range [][]string{{"secret", "verify", "extra"}, {"secret", "verify", "--kek", "x"}, {"secret", "verify", "--batch-size", "5"}, {"secret", "verifyy"}} {
		if _, err := parseArgs(a); !errors.Is(err, errInvalidArguments) {
			t.Fatalf("accepted %v", a)
		}
	}
}

func TestVerifyDoesNotRequireTerminal(t *testing.T) {
	var out bytes.Buffer
	pre := func() (terminalSession, error) { t.Fatal("terminal preflight ran for verify"); return nil, nil }
	exec := func(context.Context, command) (string, error) { return "fixed", nil }
	if err := runWithOutput(command{action: "verify"}, pre, exec, &out); err != nil || out.String() != "fixed\n" {
		t.Fatalf("%q %v", out.String(), err)
	}
	// Other actions still preflight.
	if err := runWithOutput(command{action: "rewrap"}, func() (terminalSession, error) { return nil, userError("interactive terminal required") }, exec, &out); err == nil {
		t.Fatal("rewrap skipped preflight")
	}
}

// init-active-kek reads no secret value, so the installer (no terminal) may run it; the other
// mutating actions still need one.
func TestInitActiveKEKDoesNotRequireTerminal(t *testing.T) {
	var out bytes.Buffer
	pre := func() (terminalSession, error) {
		t.Fatal("terminal preflight ran for init-active-kek")
		return nil, nil
	}
	exec := func(context.Context, command) (string, error) { return "active secret key initialized", nil }
	if err := runWithOutput(command{action: "init-active-kek", oldKEK: "kek-1"}, pre, exec, &out); err != nil || out.String() != "active secret key initialized\n" {
		t.Fatalf("%q %v", out.String(), err)
	}
	for _, action := range []string{"put", "replace", "rewrap", "can-retire"} {
		if err := runWithOutput(command{action: action}, func() (terminalSession, error) { return nil, userError("interactive terminal required") }, exec, &out); err == nil {
			t.Fatalf("%s skipped the terminal preflight", action)
		}
	}
}

func TestVerifyOutputAndFailureExit(t *testing.T) {
	operator := &fakeSecretOperator{verifyResult: secret.VerifyResult{Checked: 3, Failed: 1, Failures: []secret.VerifyFailure{{VersionID: "version-2", KEKID: "kek-a", Reason: secret.VerifyKEKMissing}}}}
	deps := testSecretDeps(t, operator)
	got, err := executeWith(context.Background(), command{action: "verify"}, deps)
	var vf verifyFailedError
	if !errors.As(err, &vf) || got != "" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	want := "secret verify completed checked=3 failed=1\nfailed version=version-2 kek=kek-a reason=kek_missing"
	if string(vf) != want {
		t.Fatalf("%q", string(vf))
	}
	var out bytes.Buffer
	err = runWithOutput(command{action: "verify"}, nil, func(context.Context, command) (string, error) { return "", vf }, &out)
	if err == nil || out.String() != want+"\n" || strings.Contains(errorMessage(err), "version-2") {
		t.Fatalf("out=%q err=%v msg=%q", out.String(), err, errorMessage(err))
	}
	operator.verifyResult = secret.VerifyResult{Checked: 2}
	if got, err = executeWith(context.Background(), command{action: "verify"}, deps); err != nil || got != "secret verify completed checked=2 failed=0" {
		t.Fatalf("%q %v", got, err)
	}
	operator.verifyErr = secret.ErrGenerationMismatch
	if _, err = executeWith(context.Background(), command{action: "verify"}, deps); err == nil || errorMessage(err) != "secret operation failed: database generation does not match the key manifest" {
		t.Fatalf("%v %q", err, errorMessage(err))
	}
	operator.verifyErr = errors.New("secret-bearing-error")
	if _, err = executeWith(context.Background(), command{action: "verify"}, deps); err == nil || strings.Contains(errorMessage(err), "secret-bearing") {
		t.Fatalf("%v", err)
	}
}

func TestVerifyReportQuotesForgedIDsAndShowsTruncation(t *testing.T) {
	operator := &fakeSecretOperator{verifyResult: secret.VerifyResult{Checked: 9, Failed: 7, Failures: []secret.VerifyFailure{
		{VersionID: "v1\nfailed version=fake reason=ok", KEKID: "k\x1b[31m", Reason: secret.VerifyKEKInvalid},
		{VersionID: "version-ok.1:a-b", KEKID: "kek-1", Reason: secret.VerifyDecryptFailed}}}}
	_, err := executeWith(context.Background(), command{action: "verify"}, testSecretDeps(t, operator))
	var vf verifyFailedError
	if !errors.As(err, &vf) {
		t.Fatal(err)
	}
	lines := strings.Split(string(vf), "\n")
	want := []string{
		"secret verify completed checked=9 failed=7",
		`failed version="v1\nfailed version=fake reason=ok" kek="k\x1b[31m" reason=kek_invalid`,
		"failed version=version-ok.1:a-b kek=kek-1 reason=decrypt_failed",
		"failures truncated listed=2",
	}
	if len(lines) != len(want) {
		t.Fatalf("%q", lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d: %q want %q", i, lines[i], want[i])
		}
	}
	if strings.ContainsRune(string(vf), 0x1b) {
		t.Fatal("raw escape in report")
	}
}
