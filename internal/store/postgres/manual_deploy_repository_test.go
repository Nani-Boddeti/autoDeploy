//go:build integration

package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/auth"
	"autodeploy/internal/deployment"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	manualSHA1  = "0123456789abcdef0123456789abcdef01234567"
	manualSHA2  = "89abcdef0123456789abcdef0123456789abcdef"
	manualNonce = "nonce-0123456789abcdef"
)

func manualFixture(t *testing.T, seed byte) (*GitHubRepository, *WebhookJobRepository, auth.Principal) {
	t.Helper()
	githubRepo, _, principal := webhookAuthority(t, "manual-bind", seed, seed+1)
	resetWebhookJobs(t, githubRepo.pool)
	return githubRepo, NewWebhookJobRepository(githubRepo.pool, NewDeploymentRepository(githubRepo.pool)), principal
}

type manualCounts struct{ intents, deployments, events, jobs, jobEvents int }

func countManual(t *testing.T, pool *pgxpool.Pool) manualCounts {
	t.Helper()
	var c manualCounts
	if err := pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM manual_deployment_intents),(SELECT count(*) FROM deployments),(SELECT count(*) FROM deployment_events),(SELECT count(*) FROM agent_jobs),(SELECT count(*) FROM agent_job_events)`).Scan(&c.intents, &c.deployments, &c.events, &c.jobs, &c.jobEvents); err != nil {
		t.Fatal(err)
	}
	return c
}

func manualIn(sha, nonce string) ManualDeployInput {
	return ManualDeployInput{ProjectID: "project", CommitSHA: sha, RequestNonce: nonce, ExpectedInstallationID: 101, ExpectedRepositoryID: 201, ExpectedRef: "refs/heads/main"}
}

func TestManualDeployHappyPathWritesOneOfEachFromBinding(t *testing.T) {
	githubRepo, repo, principal := manualFixture(t, 110)
	ctx := context.Background()
	result, err := repo.EnqueueManualDeploy(ctx, principal, manualIn(manualSHA1, manualNonce))
	if err != nil || result.Replayed || result.JobID == 0 || !strings.HasPrefix(string(result.DeploymentID), "mnd_") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if c := countManual(t, githubRepo.pool); c != (manualCounts{1, 1, 1, 1, 1}) {
		t.Fatalf("counts=%+v", c)
	}
	var env, srv, ref, sha, actor, jobDeployment string
	var repoID int64
	if err = githubRepo.pool.QueryRow(ctx, `SELECT j.environment_id,j.server_id,j.full_ref,j.commit_sha,j.repository_id,i.actor_user_id,j.deployment_id FROM agent_jobs j JOIN manual_deployment_intents i ON i.deployment_id=j.deployment_id`).Scan(&env, &srv, &ref, &sha, &repoID, &actor, &jobDeployment); err != nil {
		t.Fatal(err)
	}
	if env != "environment" || srv != "server" || ref != "refs/heads/main" || sha != manualSHA1 || repoID != 201 || actor != principal.UserID || jobDeployment != string(result.DeploymentID) {
		t.Fatalf("persisted env=%s srv=%s ref=%s sha=%s repo=%d actor=%s", env, srv, ref, sha, repoID, actor)
	}
}

func TestManualDeployReplayAndNonceConflict(t *testing.T) {
	githubRepo, repo, principal := manualFixture(t, 112)
	ctx := context.Background()
	first, err := repo.EnqueueManualDeploy(ctx, principal, manualIn(manualSHA1, manualNonce))
	if err != nil {
		t.Fatal(err)
	}
	again, err := repo.EnqueueManualDeploy(ctx, principal, manualIn(manualSHA1, manualNonce))
	if err != nil || !again.Replayed || again.DeploymentID != first.DeploymentID || again.JobID != first.JobID {
		t.Fatalf("replay=%+v err=%v", again, err)
	}
	if c := countManual(t, githubRepo.pool); c != (manualCounts{1, 1, 1, 1, 1}) {
		t.Fatalf("replay wrote rows: %+v", c)
	}
	if _, err = repo.EnqueueManualDeploy(ctx, principal, manualIn(manualSHA2, manualNonce)); !errors.Is(err, ErrManualDeployConflict) {
		t.Fatalf("different sha same nonce err=%v", err)
	}
	if c := countManual(t, githubRepo.pool); c != (manualCounts{1, 1, 1, 1, 1}) {
		t.Fatalf("conflict wrote rows: %+v", c)
	}
}

func TestManualDeployInFlightThenTerminalAllowsNewNonce(t *testing.T) {
	githubRepo, repo, principal := manualFixture(t, 114)
	ctx := context.Background()
	first, err := repo.EnqueueManualDeploy(ctx, principal, manualIn(manualSHA1, manualNonce))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.EnqueueManualDeploy(ctx, principal, manualIn(manualSHA1, "other-nonce-0123456789")); !errors.Is(err, ErrManualDeployInFlight) {
		t.Fatalf("in-flight err=%v", err)
	}
	if c := countManual(t, githubRepo.pool); c != (manualCounts{1, 1, 1, 1, 1}) {
		t.Fatalf("in-flight wrote rows: %+v", c)
	}
	// A different commit is not in flight.
	if _, err = repo.EnqueueManualDeploy(ctx, principal, manualIn(manualSHA2, "third-nonce-0123456789")); err != nil {
		t.Fatalf("different sha err=%v", err)
	}
	deployments := NewDeploymentRepository(githubRepo.pool)
	d, err := deployments.GetByID(ctx, first.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if d, err = d.Transition(deployment.StatusCancelled, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err = deployments.Save(ctx, d); err != nil {
		t.Fatal(err)
	}
	next, err := repo.EnqueueManualDeploy(ctx, principal, manualIn(manualSHA1, "other-nonce-0123456789"))
	if err != nil || next.DeploymentID == first.DeploymentID {
		t.Fatalf("after terminal result=%+v err=%v", next, err)
	}
}

func TestManualDeployValidationBeforeSQL(t *testing.T) {
	_, repo, principal := manualFixture(t, 116)
	ctx := context.Background()
	for name, in := range map[string]ManualDeployInput{
		"short nonce":              manualIn(manualSHA1, "short"),
		"bad nonce":                manualIn(manualSHA1, "nonce with spaces!!!"),
		"upper sha":                manualIn(strings.ToUpper(manualSHA1), manualNonce),
		"zero sha":                 manualIn(strings.Repeat("0", 40), manualNonce),
		"empty proj":               {CommitSHA: manualSHA1, RequestNonce: manualNonce, ExpectedInstallationID: 101, ExpectedRepositoryID: 201, ExpectedRef: "refs/heads/main"},
		"no expected installation": {ProjectID: "project", CommitSHA: manualSHA1, RequestNonce: manualNonce, ExpectedRepositoryID: 201, ExpectedRef: "refs/heads/main"},
		"no expected repository":   {ProjectID: "project", CommitSHA: manualSHA1, RequestNonce: manualNonce, ExpectedInstallationID: 101, ExpectedRef: "refs/heads/main"},
		"no expected ref":          {ProjectID: "project", CommitSHA: manualSHA1, RequestNonce: manualNonce, ExpectedInstallationID: 101, ExpectedRepositoryID: 201},
	} {
		if _, err := repo.EnqueueManualDeploy(ctx, principal, in); !errors.Is(err, ErrManualDeployInvalid) {
			t.Fatalf("%s err=%v", name, err)
		}
	}
	bad := principal
	bad.Role = ""
	if _, err := repo.EnqueueManualDeploy(ctx, bad, manualIn(manualSHA1, manualNonce)); !errors.Is(err, ErrManualDeployUnauthorized) {
		t.Fatalf("bad principal err=%v", err)
	}
}

func TestManualDeployDeniedChainWritesNothing(t *testing.T) {
	ctx := context.Background()
	cases := map[string]string{
		"inactive app":           `UPDATE github_app_registrations SET active=false WHERE id=1`,
		"suspended installation": `UPDATE github_installations SET suspended=true WHERE installation_id=101`,
		"inactive installation":  `UPDATE github_installations SET active=false WHERE installation_id=101`,
		"inactive repository":    `UPDATE github_repositories SET active=false WHERE repository_id=201`,
		"inactive binding":       `UPDATE github_project_bindings SET active=false WHERE project_id='project'`,
		"unbound":                `DELETE FROM github_project_bindings WHERE project_id='project'`,
	}
	seed := byte(120)
	for name, mutation := range cases {
		seed += 2
		githubRepo, repo, principal := manualFixture(t, seed)
		if _, err := githubRepo.pool.Exec(ctx, mutation); err != nil {
			t.Fatalf("%s setup: %v", name, err)
		}
		if _, err := repo.EnqueueManualDeploy(ctx, principal, manualIn(manualSHA1, manualNonce)); !errors.Is(err, ErrManualDeployUnauthorized) {
			t.Fatalf("%s enqueue err=%v", name, err)
		}
		if _, _, _, err := repo.ManualDeployTarget(ctx, principal, "project"); !errors.Is(err, ErrManualDeployUnauthorized) {
			t.Fatalf("%s target err=%v", name, err)
		}
		if c := countManual(t, githubRepo.pool); c != (manualCounts{}) {
			t.Fatalf("%s wrote rows: %+v", name, c)
		}
	}
}

// secondAdminPrincipal creates another admin user with a live session in the
// same owner, so two enqueues share no session or user row lock.
func secondAdminPrincipal(t *testing.T, pool *pgxpool.Pool, owner string) auth.Principal {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,owner_id,username,role,password_phc,password_policy_revision) VALUES('user-2',$1,'second-admin','owner',repeat('x',68),1)`, owner); err != nil {
		t.Fatal(err)
	}
	token := testToken(t, 201)
	if _, err := NewAuthRepository(pool).CreateSession(ctx, "session-2", token, LoginUser{ID: "user-2", OwnerID: owner, AuthRevision: 1, Active: true}, nil, "login-2", "limit-2"); err != nil {
		t.Fatal(err)
	}
	p, err := NewAuthRepository(pool).ValidateSession(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// otherOwnerPrincipal creates a second owner with a live session.
func otherOwnerPrincipal(t *testing.T, pool *pgxpool.Pool) auth.Principal {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO owners(id) VALUES('owner-b')`,
		`INSERT INTO users(id,owner_id,username,role,password_phc,password_policy_revision) VALUES('user-b','owner-b','owner-b-admin','owner',repeat('x',68),1)`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	token := testToken(t, 200)
	if _, err := NewAuthRepository(pool).CreateSession(ctx, "session-b", token, LoginUser{ID: "user-b", OwnerID: "owner-b", AuthRevision: 1, Active: true}, nil, "login-b", "limit-b"); err != nil {
		t.Fatal(err)
	}
	p, err := NewAuthRepository(pool).ValidateSession(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestManualDeployCrossOwnerDeniedWithZeroRows(t *testing.T) {
	githubRepo, repo, _ := manualFixture(t, 140)
	ctx := context.Background()
	other := otherOwnerPrincipal(t, githubRepo.pool)
	if _, err := repo.EnqueueManualDeploy(ctx, other, manualIn(manualSHA1, manualNonce)); !errors.Is(err, ErrManualDeployUnauthorized) {
		t.Fatalf("cross-owner enqueue err=%v", err)
	}
	if _, _, _, err := repo.ManualDeployTarget(ctx, other, "project"); !errors.Is(err, ErrManualDeployUnauthorized) {
		t.Fatalf("cross-owner target err=%v", err)
	}
	if c := countManual(t, githubRepo.pool); c != (manualCounts{}) {
		t.Fatalf("cross-owner wrote rows: %+v", c)
	}
}

func TestManualDeployTargetReturnsBindingFacts(t *testing.T) {
	_, repo, principal := manualFixture(t, 142)
	installationID, repositoryID, ref, err := repo.ManualDeployTarget(context.Background(), principal, "project")
	if err != nil || installationID != 101 || repositoryID != 201 || ref != "refs/heads/main" {
		t.Fatalf("target=%d %d %s err=%v", installationID, repositoryID, ref, err)
	}
	if _, _, _, err = repo.ManualDeployTarget(context.Background(), principal, "missing-project"); !errors.Is(err, ErrManualDeployUnauthorized) {
		t.Fatalf("unknown project err=%v", err)
	}
}

func TestManualDeployInjectedFailureRollsBackEveryStage(t *testing.T) {
	githubRepo, _, principal := manualFixture(t, 144)
	ctx := context.Background()
	for _, stage := range []string{"manual_intent", "job", "deployment", "deployment_event", "job_event", "manual_done"} {
		repo := NewWebhookJobRepository(githubRepo.pool, NewDeploymentRepository(githubRepo.pool))
		repo.failAfter = func(current string) error {
			if current == stage {
				return errors.New("injected")
			}
			return nil
		}
		if _, err := repo.EnqueueManualDeploy(ctx, principal, manualIn(manualSHA1, "failure-nonce-"+stage)); err == nil {
			t.Fatalf("stage %s succeeded", stage)
		}
		if c := countManual(t, githubRepo.pool); c != (manualCounts{}) {
			t.Fatalf("stage %s persisted %+v", stage, c)
		}
	}
}

// TestManualDeployContentionWithInstallationDisable holds a real row lock on
// the installation, proves the enqueue is blocked on it (pg_stat_activity),
// then commits the disable and expects the enqueue to be refused.
func TestManualDeployContentionWithInstallationDisable(t *testing.T) {
	githubRepo, repo, principal := manualFixture(t, 146)
	ctx := context.Background()
	disable, err := githubRepo.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer disable.Rollback(ctx) //nolint:errcheck
	if _, err = disable.Exec(ctx, `UPDATE github_installations SET active=false,suspended=true WHERE installation_id=101`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, e := repo.EnqueueManualDeploy(ctx, principal, manualIn(manualSHA1, manualNonce))
		done <- e
	}()
	waitForLockWaiters(t, ctx, githubRepo.pool, "FROM github_installations", 1)
	select {
	case e := <-done:
		t.Fatalf("enqueue finished while installation row was locked: %v", e)
	default:
	}
	if err = disable.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-done:
		if !errors.Is(e, ErrManualDeployUnauthorized) {
			t.Fatalf("enqueue after disable err=%v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("enqueue did not return after disable committed")
	}
	if c := countManual(t, githubRepo.pool); c != (manualCounts{}) {
		t.Fatalf("refused enqueue wrote rows: %+v", c)
	}
}

// TestManualDeployConcurrentCallsSameCommitCreateOne races two different
// nonces for one commit; the advisory serialization plus the in-flight check
// must leave exactly one deployment.
func TestManualDeployConcurrentCallsSameCommitCreateOne(t *testing.T) {
	githubRepo, repo, principal := manualFixture(t, 148)
	ctx := context.Background()
	errs := make(chan error, 2)
	for _, nonce := range []string{"race-nonce-aaaaaaaaaaaa", "race-nonce-bbbbbbbbbbbb"} {
		go func() {
			_, e := repo.EnqueueManualDeploy(ctx, principal, manualIn(manualSHA1, nonce))
			errs <- e
		}()
	}
	var ok, inflight int
	for range 2 {
		switch e := <-errs; {
		case e == nil:
			ok++
		case errors.Is(e, ErrManualDeployInFlight):
			inflight++
		default:
			t.Fatalf("unexpected err=%v", e)
		}
	}
	if ok != 1 || inflight != 1 {
		t.Fatalf("ok=%d inflight=%d", ok, inflight)
	}
	if c := countManual(t, githubRepo.pool); c != (manualCounts{1, 1, 1, 1, 1}) {
		t.Fatalf("counts=%+v", c)
	}
}

// TestManualDeployConcurrentSameNonceReplays races the same request twice.
func TestManualDeployConcurrentSameNonceReplays(t *testing.T) {
	githubRepo, repo, principal := manualFixture(t, 150)
	ctx := context.Background()
	type out struct {
		r ManualDeployResult
		e error
	}
	ch := make(chan out, 2)
	for range 2 {
		go func() {
			r, e := repo.EnqueueManualDeploy(ctx, principal, manualIn(manualSHA1, manualNonce))
			ch <- out{r, e}
		}()
	}
	a, b := <-ch, <-ch
	if a.e != nil || b.e != nil || a.r.DeploymentID != b.r.DeploymentID || a.r.JobID != b.r.JobID {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
	if c := countManual(t, githubRepo.pool); c != (manualCounts{1, 1, 1, 1, 1}) {
		t.Fatalf("counts=%+v", c)
	}
}

func waitForAdvisoryWaiters(t *testing.T, ctx context.Context, pool *pgxpool.Pool, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted AND ((classid::bigint<<32)|objid::bigint)=hashtextextended($1,0)`, key).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d ungranted advisory lock requests; saw %d", want, count)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestManualDeployInFlightCheckRunsAfterAdvisoryLock holds the exact advisory
// lock key in a side transaction, proves (pg_locks) that two enqueues by different
// admins are both queued on it, then releases and expects exactly one
// deployment: the in-flight check must run after the lock is granted.
func TestManualDeployInFlightCheckRunsAfterAdvisoryLock(t *testing.T) {
	githubRepo, repo, principal := manualFixture(t, 152)
	ctx := context.Background()
	holder, err := githubRepo.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	key := manualLockKey("environment", manualSHA1)
	if _, err = holder.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
		t.Fatal(err)
	}
	// Two different admins of one owner share no session/user row lock, so both
	// reach the advisory lock and only it can serialise them.
	second := secondAdminPrincipal(t, githubRepo.pool, principal.OwnerID)
	errs := make(chan error, 2)
	for i, p := range []auth.Principal{principal, second} {
		nonce := []string{"lock-nonce-aaaaaaaaaaaa", "lock-nonce-bbbbbbbbbbbb"}[i]
		go func() {
			_, e := repo.EnqueueManualDeploy(ctx, p, manualIn(manualSHA1, nonce))
			errs <- e
		}()
	}
	waitForAdvisoryWaiters(t, ctx, githubRepo.pool, key, 2)
	if c := countManual(t, githubRepo.pool); c != (manualCounts{}) {
		t.Fatalf("rows written while the advisory lock was held: %+v", c)
	}
	if err = holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var ok, inflight int
	for range 2 {
		switch e := <-errs; {
		case e == nil:
			ok++
		case errors.Is(e, ErrManualDeployInFlight):
			inflight++
		default:
			t.Fatalf("unexpected err=%v", e)
		}
	}
	if ok != 1 || inflight != 1 {
		t.Fatalf("ok=%d inflight=%d", ok, inflight)
	}
	if c := countManual(t, githubRepo.pool); c != (manualCounts{1, 1, 1, 1, 1}) {
		t.Fatalf("counts=%+v", c)
	}
}

// TestManualDeployRebindRaceFailsClosed holds an uncommitted rebind of the
// binding row, proves the enqueue is blocked on the binding lock, commits, and
// expects a refusal with zero rows.
func TestManualDeployRebindRaceFailsClosed(t *testing.T) {
	cases := map[string]string{
		"ref changed": `UPDATE github_project_bindings SET deploy_ref='refs/heads/other' WHERE project_id='project'`,
		"deactivated": `UPDATE github_project_bindings SET active=false WHERE project_id='project'`,
		"unbound":     `DELETE FROM github_project_bindings WHERE project_id='project'`,
	}
	seed := byte(154)
	for name, mutation := range cases {
		seed += 2
		githubRepo, repo, principal := manualFixture(t, seed)
		ctx := context.Background()
		side, err := githubRepo.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = side.Exec(ctx, mutation); err != nil {
			side.Rollback(ctx) //nolint:errcheck
			t.Fatalf("%s setup: %v", name, err)
		}
		done := make(chan error, 1)
		go func() {
			_, e := repo.EnqueueManualDeploy(ctx, principal, manualIn(manualSHA1, manualNonce))
			done <- e
		}()
		waitForLockWaiters(t, ctx, githubRepo.pool, "FROM github_project_bindings WHERE owner_id", 1)
		if err = side.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case e := <-done:
			if !errors.Is(e, ErrManualDeployConflict) && !errors.Is(e, ErrManualDeployUnauthorized) {
				t.Fatalf("%s: err=%v", name, e)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: enqueue did not return", name)
		}
		if c := countManual(t, githubRepo.pool); c != (manualCounts{}) {
			t.Fatalf("%s wrote rows: %+v", name, c)
		}
	}
}

func TestManualDeployExpectedValuesMismatchConflicts(t *testing.T) {
	githubRepo, repo, principal := manualFixture(t, 170)
	ctx := context.Background()
	for name, mutate := range map[string]func(*ManualDeployInput){
		"installation": func(in *ManualDeployInput) { in.ExpectedInstallationID = 999 },
		"repository":   func(in *ManualDeployInput) { in.ExpectedRepositoryID = 999 },
		"ref":          func(in *ManualDeployInput) { in.ExpectedRef = "refs/heads/other" },
	} {
		in := manualIn(manualSHA1, manualNonce)
		mutate(&in)
		if _, err := repo.EnqueueManualDeploy(ctx, principal, in); !errors.Is(err, ErrManualDeployConflict) {
			t.Fatalf("%s mismatch err=%v", name, err)
		}
		if c := countManual(t, githubRepo.pool); c != (manualCounts{}) {
			t.Fatalf("%s mismatch wrote rows: %+v", name, c)
		}
	}
}

func TestManualDeployHealthyCommitMayBeRedeployed(t *testing.T) {
	githubRepo, repo, principal := manualFixture(t, 172)
	ctx := context.Background()
	first, err := repo.EnqueueManualDeploy(ctx, principal, manualIn(manualSHA1, manualNonce))
	if err != nil {
		t.Fatal(err)
	}
	deployments := NewDeploymentRepository(githubRepo.pool)
	d, err := deployments.GetByID(ctx, first.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range deployment.ActiveStatuses()[1:] {
		if d, err = d.Transition(status, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err = deployments.Save(ctx, d); err != nil {
			t.Fatal(err)
		}
		// Every active phase still blocks a second deploy of the commit.
		if _, err = repo.EnqueueManualDeploy(ctx, principal, manualIn(manualSHA1, "blocked-"+string(status)+"-nonce")); !errors.Is(err, ErrManualDeployInFlight) {
			t.Fatalf("status %s err=%v", status, err)
		}
	}
	if d, err = d.Transition(deployment.StatusHealthy, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err = deployments.Save(ctx, d); err != nil {
		t.Fatal(err)
	}
	next, err := repo.EnqueueManualDeploy(ctx, principal, manualIn(manualSHA1, "redeploy-nonce-0123456789"))
	if err != nil || next.DeploymentID == first.DeploymentID {
		t.Fatalf("healthy redeploy result=%+v err=%v", next, err)
	}
}

// TestManualDeployDatabaseErrorIsNotUnauthorized cancels the context while the
// enqueue is blocked on the installation lock: the operational failure must not
// be reported as an authorization denial.
func TestManualDeployDatabaseErrorIsNotUnauthorized(t *testing.T) {
	githubRepo, repo, principal := manualFixture(t, 174)
	bg := context.Background()
	side, err := githubRepo.pool.Begin(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer side.Rollback(bg) //nolint:errcheck
	if _, err = side.Exec(bg, `UPDATE github_installations SET active=active WHERE installation_id=101`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, e := repo.EnqueueManualDeploy(ctx, principal, manualIn(manualSHA1, manualNonce))
		done <- e
	}()
	waitForLockWaiters(t, bg, githubRepo.pool, "FROM github_installations", 1)
	cancel()
	select {
	case e := <-done:
		if e == nil || errors.Is(e, ErrManualDeployUnauthorized) || errors.Is(e, ErrManualDeployConflict) || errors.Is(e, ErrManualDeployInvalid) {
			t.Fatalf("cancelled enqueue err=%v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("enqueue did not return after cancel")
	}
	cctx, ccancel := context.WithCancel(bg)
	ccancel()
	if _, _, _, err = repo.ManualDeployTarget(cctx, principal, "project"); err == nil || errors.Is(err, ErrManualDeployUnauthorized) {
		t.Fatalf("cancelled target err=%v", err)
	}
}
