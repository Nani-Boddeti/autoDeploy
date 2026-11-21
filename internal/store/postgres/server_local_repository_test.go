//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"

	"autodeploy/internal/server"
)

func TestEnrollLocalServerMarkerOwnerScopeAndEnrolledShortCircuit(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	owner, err := repo.LocalOwner(ctx)
	if err != nil || owner.OwnerID != principal.OwnerID || owner.UserID != principal.UserID {
		t.Fatalf("owner=%+v err=%v", owner, err)
	}
	digestOf := func(b byte) [32]byte {
		tok, _ := server.NewEnrollmentToken(nil)
		d, _ := server.EnrollmentTokenDigest(tok)
		d[0] ^= b
		return d
	}
	// A UI-registered same-name server is never adopted.
	_, ui := registerTestServer(t, repo, principal, "server_ui", 3)
	res, err := repo.EnrollLocalServer(ctx, "", ui.Name, "server_local", digestOf(1), 900)
	if err != nil || res.ServerID != "server_local" || res.Enrolled {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	// The local server is found again by marker+name (crash recovery), not duplicated.
	res, err = repo.EnrollLocalServer(ctx, "", ui.Name, "server_other", digestOf(2), 900)
	if err != nil || res.ServerID != "server_local" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	// Enroll it for real, then an enrolled server is reported, nothing minted.
	tok, _ := server.NewEnrollmentToken(nil)
	d, _ := server.EnrollmentTokenDigest(tok)
	if _, err = repo.EnrollLocalServer(ctx, "server_local", "x", "unused", d, 900); err != nil {
		t.Fatal(err)
	}
	cred, _ := server.NewCredentialToken(nil)
	cd, _ := server.CredentialTokenDigest(cred)
	if _, err = repo.Enroll(ctx, "server_local", d, cd); err != nil {
		t.Fatal(err)
	}
	if res, err = repo.EnrollLocalServer(ctx, "server_local", "x", "unused", digestOf(3), 900); err != nil || !res.Enrolled || res.ServerID != "server_local" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	// Name lookup never adopts an enrolled local server.
	if res, err = repo.EnrollLocalServer(ctx, "", ui.Name, "server_third", digestOf(4), 900); err != nil || res.ServerID != "server_third" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	// Another owner's server is invisible to a recorded id.
	other := secondOwnerPrincipal(t, pool, NewAuthRepository(pool), 77)
	_, foreign := registerTestServer(t, repo, other, "server_foreign", 4)
	if _, err = repo.EnrollLocalServer(ctx, foreign.ID, "x", "unused", digestOf(5), 900); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("foreign server visible: %v", err)
	}
}

func TestLocalOwnerRefusedBeforeBootstrap(t *testing.T) {
	pool := authIntegrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE auth_audit_events, admin_sessions, auth_throttle_buckets, auth_username_throttles, auth_throttle_state, users, installation_auth, owners RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installation_auth(id) VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := NewServerRepository(pool).LocalOwner(ctx); !errors.Is(err, ErrNoAdministrator) {
		t.Fatalf("err=%v", err)
	}
}

func TestLocalOwnerInactiveWhenAdministratorDisabled(t *testing.T) {
	pool, repo, _, principal := freshServerRepository(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE users SET status='disabled' WHERE id=$1`, principal.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LocalOwner(ctx); !errors.Is(err, ErrOwnerInactive) {
		t.Fatalf("err=%v", err)
	}
}
