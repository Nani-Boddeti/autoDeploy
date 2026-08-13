package gitcheckout

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"autodeploy/internal/checkoutcontain"
	"autodeploy/internal/checkoutquota"
	"autodeploy/internal/githubapp"

	"golang.org/x/sys/unix"
)

const qaCommitSHA = "0123456789abcdef0123456789abcdef01234567"

type qaStore struct {
	closed   int
	closeErr error
}

func (s *qaStore) ReserveCheckout(context.Context, string, string, string) (githubapp.CheckoutReservation, error) {
	return githubapp.CheckoutReservation{}, errors.New("unused")
}
func (s *qaStore) BeginMintAttempt(context.Context, string) (githubapp.CheckoutBinding, error) {
	return githubapp.CheckoutBinding{OwnerID: "owner", ProjectID: "project", InstallationID: 1, RepositoryID: 2, AuthorizationRevision: 1, CloneURL: "https://github.com/owner/repo.git", CommitSHA: qaCommitSHA, ReservationID: "reservation"}, nil
}
func (*qaStore) ActivateMintAttempt(context.Context, string, githubapp.CheckoutBinding) error {
	return nil
}
func (*qaStore) StartCheckout(context.Context, string, githubapp.CheckoutBinding) error { return nil }
func (s *qaStore) CloseCheckout(context.Context, string) error                          { s.closed++; return s.closeErr }

type qaProvider struct {
	revoked   int
	revokeErr error
}

func (*qaProvider) GetApp(context.Context) (githubapp.App, error) {
	return githubapp.App{}, errors.New("unused")
}
func (*qaProvider) GetInstallation(context.Context, int64) (githubapp.Installation, error) {
	return githubapp.Installation{}, errors.New("unused")
}
func (*qaProvider) ListInstallationRepositories(context.Context, int64) ([]githubapp.Repository, error) {
	return nil, errors.New("unused")
}
func (*qaProvider) MintDiscoveryToken(context.Context, int64) (githubapp.Token, error) {
	return githubapp.Token{}, errors.New("unused")
}
func (*qaProvider) MintCheckoutToken(context.Context, int64, []int64) (githubapp.Token, error) {
	return githubapp.Token{Value: "qa-runner-secret-sentinel", ExpiresAt: time.Now().Add(time.Minute), RepositoryIDs: []int64{2}, Contents: "read", Permissions: map[string]string{"contents": "read"}}, nil
}
func (p *qaProvider) RevokeToken(context.Context, string) error { p.revoked++; return p.revokeErr }
func qaLease(t *testing.T, s *qaStore, p *qaProvider) *githubapp.CheckoutLease {
	t.Helper()
	service, err := githubapp.NewCheckoutService(s, p)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := service.Activate(context.Background(), "reservation")
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

type fakeQuota struct {
	alloc                               checkoutquota.Allocation
	allocateErr, resolveErr, releaseErr error
	releaseErrs                         []error
	calls                               []string
}

func (q *fakeQuota) Allocate(context.Context, checkoutquota.Limits) (checkoutquota.Allocation, error) {
	q.calls = append(q.calls, "allocate")
	return q.alloc, q.allocateErr
}
func (q *fakeQuota) ResolveAllocation(context.Context, checkoutquota.Allocation) (checkoutquota.Allocation, error) {
	q.calls = append(q.calls, "resolve")
	return q.alloc, q.resolveErr
}
func (q *fakeQuota) ReleaseAndCleanup(context.Context, checkoutquota.Allocation) error {
	q.calls = append(q.calls, "release")
	if len(q.releaseErrs) > 0 {
		err := q.releaseErrs[0]
		q.releaseErrs = q.releaseErrs[1:]
		return err
	}
	return q.releaseErr
}

type fakeSpawn struct {
	requests       []checkoutcontain.SpawnRequest
	beforeAllocate bool
	quota          *fakeQuota
	fail           checkoutcontain.GitOperation
	secret         string
}

func (s *fakeSpawn) Run(ctx context.Context, r checkoutcontain.SpawnRequest) (checkoutcontain.SpawnResult, error) {
	if len(s.quota.calls) == 0 {
		s.beforeAllocate = true
	}
	s.requests = append(s.requests, r)
	defer closeFDs(r.WorkspaceFD, r.StdoutFD, r.StderrFD, r.StdoutReadFD, r.StderrReadFD, r.AskpassReadFD)
	if err := ctx.Err(); err != nil {
		return checkoutcontain.SpawnResult{}, err
	}
	if s.secret != "" {
		_, _ = io.WriteString(r.Stdout, s.secret)
	}
	switch r.Operation {
	case checkoutcontain.GitRevParseFetchHead, checkoutcontain.GitRevParseHead:
		_, _ = io.WriteString(r.Stdout, qaCommitSHA+"\n")
	case checkoutcontain.GitLSTree:
		_, _ = io.WriteString(r.Stdout, "100644 blob x\tREADME.md\n")
	case checkoutcontain.GitGrepLFS:
		return checkoutcontain.SpawnResult{Class: checkoutcontain.ExitFailed, ExitCode: 1}, nil
	}
	if r.Operation == s.fail {
		return checkoutcontain.SpawnResult{Class: checkoutcontain.ExitFailed, ExitCode: 2}, nil
	}
	return checkoutcontain.SpawnResult{Class: checkoutcontain.ExitSuccess}, nil
}
func qaAllocation(t *testing.T) checkoutquota.Allocation {
	t.Helper()
	fd, err := unix.Open(t.TempDir(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	return checkoutquota.Allocation{Workspace: fd, Handle: [16]byte{1}}
}
func qaRunner(t *testing.T, q *fakeQuota, s *fakeSpawn) *Runner {
	t.Helper()
	r, err := NewRunner(RunnerConfig{Quota: q, Spawn: s, QuotaLimits: checkoutquota.Limits{Bytes: 1, Inodes: 1}, SpawnLimits: checkoutcontain.SpawnLimits{TimeoutSeconds: 1, MaxOutput: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRunnerUsesQuotaThenFixedTypedOperations(t *testing.T) {
	q := &fakeQuota{alloc: qaAllocation(t)}
	s := &fakeSpawn{quota: q}
	result, err := qaRunner(t, q, s).Checkout(context.Background(), qaLease(t, &qaStore{}, &qaProvider{}))
	if err != nil {
		t.Fatal(err)
	}
	if s.beforeAllocate {
		t.Fatal("spawn preceded quota allocation")
	}
	want := []checkoutcontain.GitOperation{checkoutcontain.GitInit, checkoutcontain.GitRemoteAddOrigin, checkoutcontain.GitFetchExact, checkoutcontain.GitRevParseFetchHead, checkoutcontain.GitCheckoutFetchHead, checkoutcontain.GitRevParseHead, checkoutcontain.GitLSTree, checkoutcontain.GitGrepLFS}
	if len(s.requests) != len(want) {
		t.Fatalf("operations=%d", len(s.requests))
	}
	for i, op := range want {
		if s.requests[i].Operation != op {
			t.Fatalf("operation %d = %v", i, s.requests[i].Operation)
		}
	}
	if s.requests[1].RemoteURL != "https://github.com/owner/repo.git" || s.requests[2].Arg != qaCommitSHA {
		t.Fatal("authorized source was not exact")
	}
	for i, request := range s.requests {
		if (i == 2) != (request.AskpassReadFD >= 0) {
			t.Fatalf("askpass at %d", i)
		}
	}
	if err = result.Close(); err != nil {
		t.Fatal(err)
	}
	if err = result.Close(); err != nil {
		t.Fatal(err)
	}
	if got := len(q.calls); got != 2 || q.calls[0] != "allocate" || q.calls[1] != "release" {
		t.Fatalf("quota calls: %v", q.calls)
	}
}

func TestRunnerRedactsOutputAndCleansFailureWithLease(t *testing.T) {
	q := &fakeQuota{alloc: qaAllocation(t), releaseErr: errors.New("cleanup")}
	s := &fakeSpawn{quota: q, fail: checkoutcontain.GitInit, secret: "qa-runner-secret-sentinel"}
	store, provider := &qaStore{closeErr: errors.New("close")}, &qaProvider{revokeErr: errors.New("revoke")}
	_, err := qaRunner(t, q, s).Checkout(context.Background(), qaLease(t, store, provider))
	if !errors.Is(err, ErrGitFailed) || len(q.calls) != 2 || q.calls[1] != "release" || store.closed != 1 || provider.revoked != 1 {
		t.Fatalf("error=%v quota=%v close=%d revoke=%d", err, q.calls, store.closed, provider.revoked)
	}
}

func TestRunnerResolvesAndCleansIndeterminateAllocation(t *testing.T) {
	a := qaAllocation(t)
	q := &fakeQuota{alloc: a, allocateErr: &checkoutquota.IndeterminateAllocationError{Allocation: a, Err: errors.New("lost ack")}}
	s := &fakeSpawn{quota: q}
	_, err := qaRunner(t, q, s).Checkout(context.Background(), qaLease(t, &qaStore{}, &qaProvider{}))
	if !errors.Is(err, ErrGitFailed) || len(s.requests) != 0 || len(q.calls) != 3 || q.calls[1] != "resolve" || q.calls[2] != "release" {
		t.Fatalf("error=%v calls=%v spawns=%d", err, q.calls, len(s.requests))
	}
}

func TestRunnerRejectsInvalidConfig(t *testing.T) {
	if _, err := NewRunner(RunnerConfig{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("%v", err)
	}
}
func TestResultCloseIsIdempotent(t *testing.T) {
	q := &fakeQuota{alloc: qaAllocation(t)}
	r := &Result{quota: q, alloc: q.alloc}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if len(q.calls) != 1 {
		t.Fatal(q.calls)
	}
}

func TestPendingCleanupRetryKeepsLeaseErrorChain(t *testing.T) {
	a := qaAllocation(t)
	q := &fakeQuota{alloc: a, allocateErr: &checkoutquota.IndeterminateAllocationError{Allocation: a, Err: errors.New("lost ack")}, releaseErrs: []error{checkoutquota.ErrRetryable, nil}}
	s := &fakeSpawn{quota: q}
	provider := &qaProvider{revokeErr: errors.New("revoke")}
	result, err := qaRunner(t, q, s).Checkout(context.Background(), qaLease(t, &qaStore{}, provider))
	if result != nil {
		t.Fatal("successful deferred cleanup retained result")
	}
	if !errors.Is(err, ErrGitFailed) || !errors.Is(err, githubapp.ErrRevocationRequired) {
		t.Fatalf("error chain: %v", err)
	}
	var pending *CleanupError
	if errors.As(err, &pending) {
		t.Fatalf("cleanup wrapper survived successful retry: %v", err)
	}
}
