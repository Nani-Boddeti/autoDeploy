//go:build integration

package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/githubapp"
)

func freshGitHubRepository(t *testing.T) (*GitHubRepository, *AuthRepository, auth.Principal) {
	t.Helper()
	pool, authRepo, user, _ := freshAuthRepository(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE github_audit_events, github_project_bindings, github_repositories, github_installations, github_pending_handoffs, github_setup_states, github_app_registrations RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	token := testToken(t, 71)
	if _, err := authRepo.CreateSession(ctx, "github-session-"+t.Name(), token, user, nil, "github-login-"+t.Name(), "github-login-limit-"+t.Name()); err != nil {
		t.Fatal(err)
	}
	p, err := authRepo.ValidateSession(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	githubRepo := NewGitHubRepository(pool)
	if err := githubRepo.EnsureAppRegistration(ctx, githubapp.AppIdentity{AppID: 7, ClientID: "client", Slug: "app"}, githubapp.Owner{AccountID: 9, Type: "User"}); err != nil {
		t.Fatal(err)
	}
	return githubRepo, authRepo, p
}

func digest(b byte) [32]byte { var d [32]byte; d[0] = b; return d }
func installation() githubapp.Installation {
	return githubapp.Installation{ID: 101, Account: githubapp.Owner{AccountID: 9, Type: "User"}}
}
func repositories() []githubapp.Repository {
	return []githubapp.Repository{{ID: 201, OwnerID: 9, Name: "repo", FullName: "owner/repo", DefaultBranch: "main"}}
}

func TestGitHubRegistrationStateCallbackAndHandoffPersistence(t *testing.T) {
	repo, _, p := freshGitHubRepository(t)
	ctx := context.Background()
	identity := githubapp.AppIdentity{AppID: 7, ClientID: "client", Slug: "app"}
	owner := githubapp.Owner{AccountID: 9, Type: "User"}
	if err := repo.EnsureAppRegistration(ctx, identity, owner); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureAppRegistration(ctx, githubapp.AppIdentity{AppID: 8, ClientID: "client", Slug: "app"}, owner); !errors.Is(err, ErrGitHubDenied) {
		t.Fatalf("registration mutation=%v", err)
	}
	state, handoff := digest(1), digest(2)
	if err := repo.CreateSetupState(ctx, githubapp.SetupStateInput{Digest: state, Principal: p}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveCallback(ctx, state, handoff, installation()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PendingInstallation(ctx, handoff, p); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveCallback(ctx, state, digest(3), installation()); !errors.Is(err, ErrSetupStateInvalid) {
		t.Fatalf("callback replay=%v", err)
	}
	if err := repo.CompleteHandoff(ctx, handoff, p, installation(), repositories()); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteHandoff(ctx, handoff, p, installation(), repositories()); !errors.Is(err, ErrHandoffInvalid) {
		t.Fatalf("handoff replay=%v", err)
	}
	var installs, repos, events int
	if err := repo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM github_installations),(SELECT count(*) FROM github_repositories),(SELECT count(*) FROM github_audit_events)`).Scan(&installs, &repos, &events); err != nil || installs != 1 || repos != 1 || events != 3 {
		t.Fatalf("completion persistence installs=%d repos=%d events=%d err=%v", installs, repos, events, err)
	}
	var callbackID, completionID string
	if err := repo.pool.QueryRow(ctx, `SELECT id FROM github_audit_events WHERE action='callback_verified'`).Scan(&callbackID); err != nil {
		t.Fatal(err)
	}
	if err := repo.pool.QueryRow(ctx, `SELECT id FROM github_audit_events WHERE action='setup_completed'`).Scan(&completionID); err != nil {
		t.Fatal(err)
	}
	if callbackID != auditID("callback", handoff) || completionID != auditID("handoff", handoff) || callbackID == completionID {
		t.Fatalf("audit IDs callback=%q completion=%q", callbackID, completionID)
	}
}

func TestGitHubSetupRevalidatesSessionAndAuditsStart(t *testing.T) {
	repo, _, p := freshGitHubRepository(t)
	ctx := context.Background()
	state := digest(71)
	if err := repo.CreateSetupState(ctx, githubapp.SetupStateInput{Digest: state, Principal: p}); err != nil {
		t.Fatal(err)
	}
	var action, result string
	if err := repo.pool.QueryRow(ctx, `SELECT action,result FROM github_audit_events WHERE id=$1`, auditID("setup", state)).Scan(&action, &result); err != nil || action != "setup_started" || result != "success" {
		t.Fatalf("setup audit action=%q result=%q err=%v", action, result, err)
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE users SET auth_revision=auth_revision+1 WHERE id=$1 AND owner_id=$2`, p.UserID, p.OwnerID); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateSetupState(ctx, githubapp.SetupStateInput{Digest: digest(72), Principal: p}); !errors.Is(err, ErrGitHubDenied) {
		t.Fatalf("stale principal started setup: %v", err)
	}
}

func TestGitHubAuthorityMutationRequiresActiveRegistration(t *testing.T) {
	repo, _, p := freshGitHubRepository(t)
	ctx := context.Background()
	if _, err := repo.pool.Exec(ctx, `UPDATE github_app_registrations SET active=false WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateSetupState(ctx, githubapp.SetupStateInput{Digest: digest(76), Principal: p}); !errors.Is(err, ErrGitHubDenied) {
		t.Fatalf("inactive registration granted setup authority: %v", err)
	}
	if _, err := repo.pool.Exec(ctx, `DELETE FROM github_app_registrations WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateSetupState(ctx, githubapp.SetupStateInput{Digest: digest(77), Principal: p}); !errors.Is(err, ErrGitHubDenied) {
		t.Fatalf("missing registration granted setup authority: %v", err)
	}
}

func TestGitHubCallbackLeaseBoundsProviderWorkAndCanBeReleased(t *testing.T) {
	repo, _, p := freshGitHubRepository(t)
	ctx := context.Background()
	state := digest(73)
	if err := repo.CreateSetupState(ctx, githubapp.SetupStateInput{Digest: state, Principal: p}); err != nil {
		t.Fatal(err)
	}
	if err := repo.BeginCallback(ctx, state); err != nil {
		t.Fatal(err)
	}
	if err := repo.BeginCallback(ctx, state); !errors.Is(err, ErrSetupStateInvalid) {
		t.Fatalf("concurrent callback lease accepted: %v", err)
	}
	if err := repo.CancelCallback(ctx, state); err != nil {
		t.Fatal(err)
	}
	if err := repo.BeginCallback(ctx, state); err != nil {
		t.Fatalf("released callback lease was not retryable: %v", err)
	}
}

func TestGitHubDisableInstallationRevokesRepositoryAuthorityAtomically(t *testing.T) {
	repo, _, p := freshGitHubRepository(t)
	ctx := context.Background()
	state, handoff := digest(74), digest(75)
	if err := repo.CreateSetupState(ctx, githubapp.SetupStateInput{Digest: state, Principal: p}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveCallback(ctx, state, handoff, installation()); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteHandoff(ctx, handoff, p, installation(), repositories()); err != nil {
		t.Fatal(err)
	}
	if err := repo.BindRepository(ctx, p, "lifecycle_project", 201, "lifecycle-bind"); err != nil {
		t.Fatal(err)
	}
	if err := repo.DisableInstallation(ctx, p, 101, "lifecycle-disable"); err != nil {
		t.Fatal(err)
	}
	var installationActive, installationSuspended, repositoryActive, bindingActive bool
	var installationRevision, repositoryRevision, bindingRevision, disabledEvents int64
	if err := repo.pool.QueryRow(ctx, `SELECT i.active,i.suspended,i.authorization_revision,r.active,r.authorization_revision,b.active,b.authorization_revision,(SELECT count(*) FROM github_audit_events WHERE action IN ('installation_disabled','repository_disabled')) FROM github_installations i JOIN github_repositories r ON r.installation_id=i.installation_id AND r.owner_id=i.owner_id JOIN github_project_bindings b ON b.repository_id=r.repository_id AND b.owner_id=r.owner_id WHERE i.installation_id=101 AND i.owner_id=$1`, p.OwnerID).Scan(&installationActive, &installationSuspended, &installationRevision, &repositoryActive, &repositoryRevision, &bindingActive, &bindingRevision, &disabledEvents); err != nil {
		t.Fatal(err)
	}
	if installationActive || !installationSuspended || repositoryActive || bindingActive || installationRevision < 2 || repositoryRevision < 2 || bindingRevision < 2 || disabledEvents != 2 {
		t.Fatalf("authority still active installation=%t/%t/%d repository=%t/%d binding=%t/%d events=%d", installationActive, installationSuspended, installationRevision, repositoryActive, repositoryRevision, bindingActive, bindingRevision, disabledEvents)
	}
	if err := repo.DisableInstallation(ctx, p, 101, "lifecycle-disable-again"); err != nil {
		t.Fatalf("idempotent disable: %v", err)
	}
}

func TestGitHubAuditAdmissionBackfillIsolationAndRollback(t *testing.T) {
	repo, _, p := freshGitHubRepository(t)
	ctx := context.Background()
	var activeAuthority int
	if err := repo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM github_installations WHERE owner_id=$1 AND active)+(SELECT count(*) FROM github_repositories WHERE owner_id=$1 AND active)`, p.OwnerID).Scan(&activeAuthority); err != nil || activeAuthority != 0 {
		t.Fatalf("cap fixture has active authority=%d err=%v", activeAuthority, err)
	}
	// With no active authority, the dynamic admission rule permits ordinary
	// events through 100,000. The active-authority reserve is tested below.
	if _, err := repo.pool.Exec(ctx, `INSERT INTO github_audit_admission(owner_id,event_count) VALUES($1,100000)`, p.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `INSERT INTO github_audit_events(id,owner_id,action,result) VALUES('admission-two',$1,'setup_started','success')`, p.OwnerID); err == nil {
		t.Fatal("ordinary audit exceeded admission cap")
	}
	var count int
	if err := repo.pool.QueryRow(ctx, `SELECT event_count FROM github_audit_admission WHERE owner_id=$1`, p.OwnerID).Scan(&count); err != nil || count != 100000 {
		t.Fatalf("counter after cap count=%d err=%v", count, err)
	}
	if _, err := repo.pool.Exec(ctx, `INSERT INTO owners(id) VALUES('audit-other')`); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `INSERT INTO github_audit_events(id,owner_id,action,result) VALUES('admission-other','audit-other','setup_started','success')`); err != nil {
		t.Fatal(err)
	}
	if err := repo.pool.QueryRow(ctx, `SELECT event_count FROM github_audit_admission WHERE owner_id='audit-other'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("owner isolation count=%d err=%v", count, err)
	}
	tx, err := repo.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO github_audit_events(id,owner_id,action,result) VALUES('admission-duplicate','audit-other','installation_disabled','success')`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO github_audit_events(id,owner_id,action,result) VALUES('admission-duplicate','audit-other','repository_disabled','success')`); err == nil {
		t.Fatal("duplicate audit accepted")
	}
	_ = tx.Rollback(ctx)
	if err := repo.pool.QueryRow(ctx, `SELECT event_count FROM github_audit_admission WHERE owner_id='audit-other'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed audit leaked admission count=%d err=%v", count, err)
	}
	if _, err := repo.pool.Exec(ctx, `INSERT INTO github_audit_events(id,owner_id,action,result) VALUES('admission-disable','audit-other','installation_disabled','success')`); err != nil {
		t.Fatalf("reserved disable slot denied: %v", err)
	}
	if err := repo.pool.QueryRow(ctx, `SELECT event_count FROM github_audit_admission WHERE owner_id='audit-other'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("reserved counter=%d err=%v", count, err)
	}
}

func TestGitHubAuditAdmissionPreservesDynamicDisableReserve(t *testing.T) {
	repo, _, p := freshGitHubRepository(t)
	ctx := context.Background()
	state, handoff := digest(78), digest(79)
	if err := repo.CreateSetupState(ctx, githubapp.SetupStateInput{Digest: state, Principal: p}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveCallback(ctx, state, handoff, installation()); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteHandoff(ctx, handoff, p, installation(), repositories()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE github_audit_admission SET event_count=99997 WHERE owner_id=$1`, p.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `INSERT INTO github_audit_events(id,owner_id,action,result) VALUES('dynamic-ordinary-ok',$1,'setup_started','success')`, p.OwnerID); err != nil {
		t.Fatalf("ordinary event before reserve boundary: %v", err)
	}
	if _, err := repo.pool.Exec(ctx, `INSERT INTO github_audit_events(id,owner_id,action,result) VALUES('dynamic-ordinary-denied',$1,'setup_started','success')`, p.OwnerID); err == nil {
		t.Fatal("ordinary event invaded active revoke reserve")
	}
	if err := repo.DisableInstallation(ctx, p, 101, "dynamic-disable"); err != nil {
		t.Fatalf("dynamic reserve blocked later disable: %v", err)
	}
	var count int
	if err := repo.pool.QueryRow(ctx, `SELECT event_count FROM github_audit_admission WHERE owner_id=$1`, p.OwnerID).Scan(&count); err != nil || count != 100000 {
		t.Fatalf("post-disable count=%d err=%v", count, err)
	}
}

func TestGitHubStateSingleSessionCapCleanupAndConcurrentConsume(t *testing.T) {
	repo, _, p := freshGitHubRepository(t)
	ctx := context.Background()
	if _, err := repo.pool.Exec(ctx, `UPDATE github_setup_state_limits SET max_active=8 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	for i := byte(1); i <= 8; i++ {
		q := p
		q.SessionID = p.SessionID
		if err := repo.CreateSetupState(ctx, githubapp.SetupStateInput{Digest: digest(i), Principal: q}); err != nil {
			t.Fatal(err)
		}
	}
	var active int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM github_setup_states WHERE consumed_at IS NULL`).Scan(&active); err != nil || active != 1 {
		t.Fatalf("single session active=%d err=%v", active, err)
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE github_setup_states SET created_at=clock_timestamp()-interval '2 seconds', expires_at=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateSetupState(ctx, githubapp.SetupStateInput{Digest: digest(12), Principal: p}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveCallback(ctx, digest(12), digest(13), installation()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, h := range [][32]byte{digest(14), digest(15)} {
		wg.Add(1)
		go func(h [32]byte) { defer wg.Done(); results <- repo.ReserveCallback(ctx, digest(12), h, installation()) }(h)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 0 {
		t.Fatal("consumed state was reused")
	}
}

func TestGitHubBindingsOwnerForeignKeysRevisionAndAuditRollback(t *testing.T) {
	repo, _, p := freshGitHubRepository(t)
	ctx := context.Background()
	state, handoff := digest(21), digest(22)
	if err := repo.CreateSetupState(ctx, githubapp.SetupStateInput{Digest: state, Principal: p}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveCallback(ctx, state, handoff, installation()); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteHandoff(ctx, handoff, p, installation(), repositories()); err != nil {
		t.Fatal(err)
	}
	if err := repo.BindRepository(ctx, p, "opaque_project", 201, "bind-one"); err != nil {
		t.Fatal(err)
	}
	if err := repo.BindRepository(ctx, p, "opaque_project", 201, "bind-two"); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := repo.pool.QueryRow(ctx, `SELECT authorization_revision FROM github_project_bindings WHERE owner_id=$1 AND project_id='opaque_project'`, p.OwnerID).Scan(&revision); err != nil || revision != 2 {
		t.Fatalf("revision=%d err=%v", revision, err)
	}
	if _, err := repo.pool.Exec(ctx, `INSERT INTO github_project_bindings(owner_id,project_id,repository_id,installation_id) VALUES ('other','bad',201,101)`); err == nil {
		t.Fatal("cross-owner binding accepted")
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE github_audit_events SET result='failed' WHERE id='bind-one'`); err == nil {
		t.Fatal("audit update accepted")
	}
	if err := repo.BindRepository(ctx, p, "other_project", 201, "bind-two"); err == nil {
		t.Fatal("duplicate audit accepted")
	}
	var bound int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM github_project_bindings WHERE owner_id=$1`, p.OwnerID).Scan(&bound); err != nil || bound != 1 {
		t.Fatalf("audit failure did not rollback binding: %d %v", bound, err)
	}
}

func TestGitHubSchemaRejectsMalformedMetadataAndExpiredHandoff(t *testing.T) {
	repo, _, p := freshGitHubRepository(t)
	ctx := context.Background()
	if _, err := repo.pool.Exec(ctx, `INSERT INTO github_audit_events(id,owner_id,action,result,metadata) VALUES ('bad-meta',$1,'setup_started','success',jsonb_build_object('x',repeat('x',2000)))`, p.OwnerID); err == nil {
		t.Fatal("oversized metadata accepted")
	}
	state, handoff := digest(31), digest(32)
	if err := repo.CreateSetupState(ctx, githubapp.SetupStateInput{Digest: state, Principal: p}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveCallback(ctx, state, handoff, installation()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE github_pending_handoffs SET created_at=clock_timestamp()-interval '2 seconds', expires_at=clock_timestamp()-interval '1 second' WHERE digest=$1`, handoff[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PendingInstallation(ctx, handoff, p); !errors.Is(err, ErrHandoffInvalid) {
		t.Fatalf("expired handoff=%v", err)
	}
	_ = time.Second // documents time-sensitive database assertions without a client clock dependency.
}
