package githubapp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCloneURLValidTable(t *testing.T) {
	ok := "https://github.com/owner/repo.git"
	cases := map[string]bool{
		ok: true,
		"https://github.com/Own-er_1/re.po-x.git":                    true,
		"http://github.com/owner/repo.git":                           false,
		"https://github.com.evil.com/owner/repo.git":                 false,
		"https://GitHub.com/owner/repo.git":                          false,
		"https://github.com:443/owner/repo.git":                      false,
		"https://user@github.com/owner/repo.git":                     false,
		"https://user:pw@github.com/owner/repo.git":                  false,
		"https://github.com@evil.com/owner/repo.git":                 false,
		"https://github.com/owner/repo.git?x=1":                      false,
		"https://github.com/owner/repo.git?":                         false,
		"https://github.com/owner/repo.git#frag":                     false,
		"https://github.com/owner%2Fx/repo.git":                      false,
		"https://github.com/owner/re%2Fpo.git":                       false,
		"https://github.com/owner/re%70o.git":                        false,
		"https://github.com/owner/repo.git\n":                        false,
		"https://github.com/owner/re\x00po.git":                      false,
		"https://github.com/owner/re po.git":                         false,
		"https://github.com/../repo.git":                             false,
		"https://github.com/owner/...git":                            false, // repo ".." is a dot segment
		"https://github.com/owner/.git":                              false,
		"https://github.com/owner/repo/extra.git":                    false,
		"https://github.com//repo.git":                               false,
		"https://github.com/owner/repo.git/":                         false,
		"https://github.com/owner/repo":                              false,
		"https://github.com/repo.git":                                false,
		"HTTPS://github.com/owner/repo.git":                          false, // scheme must be lowercase
		"https:github.com/owner/repo.git":                            false,
		"https://github.com/" + strings.Repeat("a", 1100) + "/r.git": false,
		"": false,
	}
	for in, want := range cases {
		if got := (CheckoutBinding{CloneURL: in}).CloneURLValid(); got != want {
			t.Errorf("CloneURLValid(%q) = %v, want %v", in, got, want)
		}
	}
}

type blockingRevokeProvider struct {
	checkoutProviderStub
	deadlineSet bool
}

func (p *blockingRevokeProvider) RevokeToken(ctx context.Context, _ string) error {
	_, p.deadlineSet = ctx.Deadline()
	<-ctx.Done()
	return ctx.Err()
}

func TestRevokeIsBoundedEvenWithoutCallerDeadline(t *testing.T) {
	old := revokeTimeout
	revokeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { revokeTimeout = old })
	p := &blockingRevokeProvider{}
	l := &CheckoutLease{provider: p, token: "tok"}
	start := time.Now()
	err := l.Revoke(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || !p.deadlineSet || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v deadlineSet=%v elapsed=%v", err, p.deadlineSet, time.Since(start))
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestInvalidateLogsRevokeFailureClassOnly(t *testing.T) {
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	secretText := "provider-secret-detail"
	p := &checkoutProviderStub{revokeErr: errors.New(secretText)}
	l := &CheckoutLease{provider: p, token: "tok"}
	l.invalidate()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "revoke") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	out := buf.String()
	if !strings.Contains(out, "class=other") || strings.Contains(out, secretText) {
		t.Fatalf("log = %q", out)
	}
}
