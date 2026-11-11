package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/auth"

	"github.com/jackc/pgx/v5/pgxpool"
)

// lazyPool never dials: validation failures must return before any query.
func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func validPrincipal() auth.Principal {
	return auth.Principal{UserID: "u", OwnerID: "o", Role: auth.RoleOwner, AuthRevision: 1, SessionID: "s"}
}

func TestOperationsReadInputValidation(t *testing.T) {
	ctx := context.Background()
	repo := NewOperationsReadRepository(lazyPool(t))
	p := validPrincipal()
	long := strings.Repeat("x", 129)
	at := time.Now()

	for _, bad := range []auth.Principal{{}, {UserID: "u", OwnerID: "o", Role: auth.RoleOwner, SessionID: "s"}} {
		if _, err := repo.ListProjects(ctx, bad); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("ListProjects invalid principal = %v", err)
		}
		if _, err := repo.ListDeployments(ctx, bad, DeploymentFilter{}, DeploymentPage{}); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("ListDeployments invalid principal = %v", err)
		}
		if _, err := repo.GetDeploymentDetail(ctx, bad, "d"); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("GetDeploymentDetail invalid principal = %v", err)
		}
	}
	var nilRepo *OperationsReadRepository
	if _, err := nilRepo.ListProjects(ctx, p); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("nil repo = %v", err)
	}

	for name, tc := range map[string]struct {
		f DeploymentFilter
		p DeploymentPage
	}{
		"negative limit":    {p: DeploymentPage{Limit: -1}},
		"limit too large":   {p: DeploymentPage{Limit: MaxDeploymentPageLimit + 1}},
		"bad project":       {f: DeploymentFilter{ProjectID: long}},
		"bad environment":   {f: DeploymentFilter{EnvironmentID: "a\x00b"}},
		"zero cursor time":  {p: DeploymentPage{After: &DeploymentCursor{DeploymentID: "d"}}},
		"empty cursor id":   {p: DeploymentPage{After: &DeploymentCursor{CreatedAt: at}}},
		"overlong cursorID": {p: DeploymentPage{After: &DeploymentCursor{CreatedAt: at, DeploymentID: long}}},
	} {
		if _, err := repo.ListDeployments(ctx, p, tc.f, tc.p); !errors.Is(err, ErrOperationsInvalidInput) {
			t.Fatalf("%s: err = %v, want ErrOperationsInvalidInput", name, err)
		}
	}
	for _, id := range []string{"", long, "a\x00b", "\xff"} {
		if _, err := repo.GetDeploymentDetail(ctx, p, id); !errors.Is(err, ErrOperationsNotFound) {
			t.Fatalf("detail %q = %v, want ErrOperationsNotFound", id, err)
		}
	}
}

func TestShortCommit(t *testing.T) {
	if got := (DeploymentSummary{CommitSHA: "0123456789abcdef0123"}).ShortCommit(); got != "0123456789ab" {
		t.Fatal(got)
	}
	if got := (DeploymentSummary{CommitSHA: "abc"}).ShortCommit(); got != "abc" {
		t.Fatal(got)
	}
}
