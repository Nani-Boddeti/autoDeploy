package admincli_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/admincli"
	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

func TestBootstrapCalibratesBeforePasswordAndUsesDistinctIDs(t *testing.T) {
	events := []string{}
	repo := &fakeRepository{bootstrap: func(_ context.Context, in postgres.BootstrapInput) error {
		events = append(events, "bootstrap")
		if in.OwnerID == in.UserID || in.UserID == in.AuditID || in.OwnerID == in.AuditID {
			t.Fatal("IDs not distinct")
		}
		return nil
	}}
	terminal := &fakeTerminal{values: [][]byte{[]byte("a sufficiently long password"), []byte("a sufficiently long password")}, event: &events}
	randomness := strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32) + strings.Repeat("c", 32) + strings.Repeat("d", 16))
	dependencies := admincli.Dependencies{Repository: repo, Terminal: terminal, Random: randomness, Benchmark: func(auth.Argon2Policy) (time.Duration, error) {
		events = append(events, "benchmark")
		return 250 * time.Millisecond, nil
	}}
	if err := admincli.Bootstrap(context.Background(), "Admin", dependencies); err != nil {
		t.Fatal(err)
	}
	if events[0] != "benchmark" {
		t.Fatalf("password prompt before calibration: %v", events)
	}
}

func TestResetUsesAllAliasesAndSanitizesNotFound(t *testing.T) {
	keyA, keyB := make([]byte, 32), make([]byte, 32)
	keyA[0], keyB[0] = 1, 2
	ring := auth.UsernameThrottleKeyRing{ActiveVersion: "active", Keys: map[string][]byte{"active": keyA, "old": keyB}, RetainedVersions: []string{"old"}}
	repo := &fakeRepository{user: postgres.LoginUser{ID: "user"}, policy: auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}, reset: func(_ context.Context, _ string, _ auth.PasswordVerifier, _ string, digests [][32]byte) error {
		if len(digests) != 2 {
			t.Fatalf("digests %d", len(digests))
		}
		return nil
	}}
	deps := admincli.Dependencies{Repository: repo, Terminal: &fakeTerminal{values: [][]byte{[]byte("a sufficiently long password"), []byte("a sufficiently long password")}}, Random: strings.NewReader(strings.Repeat("x", 48)), Benchmark: fixedBenchmark}
	if err := admincli.ResetPassword(context.Background(), "admin", ring, deps); err != nil {
		t.Fatal(err)
	}
	repo.user = postgres.LoginUser{}
	if err := admincli.ResetPassword(context.Background(), "admin", ring, deps); !errors.Is(err, admincli.ErrOperationFailed) || strings.Contains(err.Error(), "user") {
		t.Fatalf("not found err %v", err)
	}
}

func TestPromptErrorsAndCalibrationBounds(t *testing.T) {
	deps := admincli.Dependencies{Repository: &fakeRepository{}, Terminal: &fakeTerminal{values: [][]byte{[]byte("different sufficiently long password"), []byte("a sufficiently long password")}}, Random: strings.NewReader(strings.Repeat("x", 96)), Benchmark: fixedBenchmark}
	if err := admincli.Bootstrap(context.Background(), "admin", deps); !errors.Is(err, admincli.ErrOperationFailed) {
		t.Fatal(err)
	}
	policy, err := admincli.Calibrate(func(p auth.Argon2Policy) (time.Duration, error) {
		if p.MemoryKiB == 48*1024 && p.Iterations == 3 {
			return 249 * time.Millisecond, nil
		}
		return time.Second, nil
	})
	if err != nil || policy.MemoryKiB != 48*1024 || policy.Iterations != 3 || policy.Lanes != 1 || policy.Revision != 1 {
		t.Fatalf("policy %#v err %v", policy, err)
	}
}

func TestBootstrapFailurePathsAreSanitized(t *testing.T) {
	password := []byte(strings.Repeat("p", 15))
	for _, terminal := range []*fakeTerminal{
		{errs: []error{errors.New("first secret")}},
		{values: [][]byte{password}, errs: []error{nil, errors.New("second secret")}},
		{values: [][]byte{[]byte(strings.Repeat("p", 14)), []byte(strings.Repeat("p", 14))}},
		{values: [][]byte{[]byte{0xff}, []byte{0xff}}},
	} {
		if err := admincli.Bootstrap(context.Background(), "admin", testDependencies(&fakeRepository{}, terminal)); !errors.Is(err, admincli.ErrOperationFailed) || err.Error() != admincli.ErrOperationFailed.Error() {
			t.Fatalf("err %v", err)
		}
	}
	for _, repo := range []*fakeRepository{
		{bootstrap: func(context.Context, postgres.BootstrapInput) error { return errors.New("database secret") }},
	} {
		fresh := []byte(strings.Repeat("p", 15))
		if err := admincli.Bootstrap(context.Background(), "admin", testDependencies(repo, &fakeTerminal{values: [][]byte{fresh, append([]byte(nil), fresh...)}})); !errors.Is(err, admincli.ErrOperationFailed) {
			t.Fatal(err)
		}
	}
	// Both password bounds are accepted through the complete bootstrap path.
	for _, password := range [][]byte{[]byte(strings.Repeat("p", 15)), []byte(strings.Repeat("p", 1024))} {
		if err := admincli.Bootstrap(context.Background(), "admin", testDependencies(&fakeRepository{}, &fakeTerminal{values: [][]byte{password, append([]byte(nil), password...)}})); err != nil {
			t.Fatalf("boundary %d: %v", len(password), err)
		}
	}
}

func TestResetFailurePathsAndFourRetainedAliases(t *testing.T) {
	keys := map[string][]byte{}
	versions := []string{"active", "v1", "v2", "v3", "v4"}
	for index, version := range versions {
		key := make([]byte, 32)
		key[0] = byte(index + 1)
		keys[version] = key
	}
	ring := auth.UsernameThrottleKeyRing{ActiveVersion: "active", Keys: keys, RetainedVersions: versions[1:]}
	password := []byte(strings.Repeat("p", 15))
	repo := &fakeRepository{user: postgres.LoginUser{ID: "user"}, policy: testPolicy(), reset: func(_ context.Context, _ string, _ auth.PasswordVerifier, _ string, got [][32]byte) error {
		if len(got) != 5 {
			t.Fatalf("got aliases %d", len(got))
		}
		return nil
	}}
	if err := admincli.ResetPassword(context.Background(), "admin", ring, testDependencies(repo, &fakeTerminal{values: [][]byte{password, append([]byte(nil), password...)}})); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []*fakeRepository{
		{user: postgres.LoginUser{}},
		{findErr: errors.New("find secret")},
		{user: postgres.LoginUser{ID: "user"}, policyErr: errors.New("policy secret")},
		{user: postgres.LoginUser{ID: "user"}, policy: auth.Argon2Policy{}},
		{user: postgres.LoginUser{ID: "user"}, policy: testPolicy(), reset: func(context.Context, string, auth.PasswordVerifier, string, [][32]byte) error {
			return errors.New("reset secret")
		}},
	} {
		fresh := []byte(strings.Repeat("p", 15))
		if err := admincli.ResetPassword(context.Background(), "admin", ring, testDependencies(repo, &fakeTerminal{values: [][]byte{fresh, append([]byte(nil), fresh...)}})); !errors.Is(err, admincli.ErrOperationFailed) {
			t.Fatal(err)
		}
	}
}

func TestRandomAndCalibrationFailures(t *testing.T) {
	password := []byte(strings.Repeat("p", 15))
	deps := testDependencies(&fakeRepository{}, &fakeTerminal{values: [][]byte{password, append([]byte(nil), password...)}})
	deps.Random = strings.NewReader("short")
	if err := admincli.Bootstrap(context.Background(), "admin", deps); !errors.Is(err, admincli.ErrOperationFailed) {
		t.Fatal(err)
	}
	password = []byte(strings.Repeat("p", 15))
	deps.Terminal = &fakeTerminal{values: [][]byte{password, append([]byte(nil), password...)}}
	deps.Random = strings.NewReader(strings.Repeat("a", 112))
	if err := admincli.Bootstrap(context.Background(), "admin", deps); !errors.Is(err, admincli.ErrOperationFailed) {
		t.Fatal(err)
	}
	for _, benchmark := range []admincli.Benchmark{
		func(auth.Argon2Policy) (time.Duration, error) { return 0, errors.New("benchmark secret") },
		func(auth.Argon2Policy) (time.Duration, error) { return -time.Millisecond, nil },
	} {
		if _, err := admincli.Calibrate(benchmark); err == nil {
			t.Fatal("accepted bad benchmark")
		}
	}
	for _, value := range []time.Duration{time.Millisecond, time.Second} {
		policy, err := admincli.Calibrate(func(auth.Argon2Policy) (time.Duration, error) { return value, nil })
		if err != nil || policy.Revision != 1 || policy.Lanes != 1 || policy.MemoryKiB < 19*1024 || policy.MemoryKiB > 64*1024 || policy.Iterations < 2 || policy.Iterations > 5 {
			t.Fatalf("policy %#v err %v", policy, err)
		}
	}
}

func testPolicy() auth.Argon2Policy {
	return auth.Argon2Policy{Revision: 1, MemoryKiB: 19 * 1024, Iterations: 2, Lanes: 1}
}
func testDependencies(repo *fakeRepository, terminal *fakeTerminal) admincli.Dependencies {
	return admincli.Dependencies{Repository: repo, Terminal: terminal, Random: strings.NewReader(strings.Repeat("a", 32) + strings.Repeat("b", 32) + strings.Repeat("c", 32) + strings.Repeat("d", 32)), Benchmark: fixedBenchmark}
}

func fixedBenchmark(auth.Argon2Policy) (time.Duration, error) { return time.Millisecond, nil }

type fakeTerminal struct {
	values [][]byte
	errs   []error
	event  *[]string
}

func (f *fakeTerminal) ReadPassword(string) ([]byte, error) {
	if f.event != nil {
		*f.event = append(*f.event, "prompt")
	}
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(f.values) == 0 {
		return nil, errors.New("read")
	}
	value := f.values[0]
	f.values = f.values[1:]
	return value, nil
}

type fakeRepository struct {
	user      postgres.LoginUser
	policy    auth.Argon2Policy
	findErr   error
	policyErr error
	bootstrap func(context.Context, postgres.BootstrapInput) error
	reset     func(context.Context, string, auth.PasswordVerifier, string, [][32]byte) error
}

func (f *fakeRepository) Bootstrap(ctx context.Context, in postgres.BootstrapInput) error {
	if f.bootstrap != nil {
		return f.bootstrap(ctx, in)
	}
	return nil
}
func (f *fakeRepository) LoadPasswordPolicy(context.Context) (auth.Argon2Policy, error) {
	return f.policy, f.policyErr
}
func (f *fakeRepository) FindLoginUserByCanonicalUsername(context.Context, string) (postgres.LoginUser, error) {
	return f.user, f.findErr
}
func (f *fakeRepository) ResetPassword(ctx context.Context, u string, v auth.PasswordVerifier, a string, d [][32]byte) error {
	if f.reset != nil {
		return f.reset(ctx, u, v, a, d)
	}
	return nil
}
