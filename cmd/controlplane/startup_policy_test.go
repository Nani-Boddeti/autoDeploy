package main

import (
	"context"
	"errors"
	"testing"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

type fakePolicyLoader struct {
	policy auth.Argon2Policy
	err    error
}

func (f fakePolicyLoader) LoadPasswordPolicy(context.Context) (auth.Argon2Policy, error) {
	return f.policy, f.err
}

func TestStartupPasswordPolicyUsesStoredPolicyWhenBootstrapped(t *testing.T) {
	stored := auth.Argon2Policy{Revision: 1, MemoryKiB: 32 * 1024, Iterations: 3, Lanes: 1}
	got, durable, err := startupPasswordPolicy(context.Background(), fakePolicyLoader{policy: stored}, func() (auth.Argon2Policy, error) {
		t.Fatal("calibrated although a stored policy exists")
		return auth.Argon2Policy{}, nil
	})
	if err != nil || got != stored || !durable {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

// Before the first administrator exists there is no stored policy; the
// control plane must still start so /setup can serve.
func TestStartupPasswordPolicyCalibratesBeforeBootstrap(t *testing.T) {
	calibrated := auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
	calls := 0
	got, durable, err := startupPasswordPolicy(context.Background(), fakePolicyLoader{err: postgres.ErrNotFound}, func() (auth.Argon2Policy, error) {
		calls++
		return calibrated, nil
	})
	if err != nil || got != calibrated || calls != 1 || durable {
		t.Fatalf("got=%+v calls=%d err=%v", got, calls, err)
	}
	if err = calibrated.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestStartupPasswordPolicyOtherErrorsStayFatal(t *testing.T) {
	boom := errors.New("database down")
	_, _, err := startupPasswordPolicy(context.Background(), fakePolicyLoader{err: boom}, func() (auth.Argon2Policy, error) {
		t.Fatal("calibrated on a non-ErrNotFound error")
		return auth.Argon2Policy{}, nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
}
