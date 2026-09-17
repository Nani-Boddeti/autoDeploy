//go:build integration

package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/deployment"
	"autodeploy/internal/queue"
)

// withCredential inserts an active credential for f.serverID and returns its
// digest, so Claim/Renew's re-validated authentication has something to
// match. newLeaseFixture itself only creates the servers row (mirroring a
// server that exists but has never enrolled), matching Decision 3's premise
// that claim/renew authenticate independently of any earlier HTTP call.
func (f leaseFixture) withCredential(t *testing.T, seed byte) [32]byte {
	t.Helper()
	d := digest(seed)
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO server_credentials(server_id,version,digest,status) VALUES ($1,1,$2,'active')`, f.serverID, d[:]); err != nil {
		t.Fatal(err)
	}
	return d
}

// insertServer adds a second servers row under the fixture's owner.
func (f leaseFixture) insertServer(t *testing.T, id string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO servers(id, owner_id, name, protocol_version, status) VALUES ($1,$2,$1,1,'active')`, id, f.ownerID); err != nil {
		t.Fatal(err)
	}
}

// insertJobAt directly inserts a queued (or, via state, retry_wait) agent_job
// row honoring every column CHECK, plus its backing deployment and initial
// enqueue event, without going through the webhook enqueue path -- this
// lets tests place jobs on arbitrary servers/environments/priorities that
// the fixed webhookInput binding cannot produce. availableAtExpr is a raw
// SQL expression (e.g. "clock_timestamp()" or
// "clock_timestamp() + interval '1 hour'").
func (f leaseFixture) insertJobAt(t *testing.T, serverID, environmentID, key string, priority int16, availableAtExpr string) int64 {
	t.Helper()
	ctx := context.Background()
	deploymentID := "deployment-" + key
	// Deployment creation uses PostgreSQL's clock, not Go's time.Now(): Claim
	// later transitions this deployment using clock_timestamp(), and any
	// host/container clock skew between a Go-clock createdAt and that later
	// DB-clock "now" could otherwise make the queued->assigned transition
	// timestamp look like it precedes creation.
	var createdAt time.Time
	if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&createdAt); err != nil {
		t.Fatal(err)
	}
	dep, err := deployment.New(deployment.Identity{DeploymentID: deployment.ID(deploymentID), ProjectID: "project", EnvironmentID: environmentID, ServerID: serverID, CommitSHA: "0123456789abcdef0123456789abcdef01234567"}, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewDeploymentRepository(f.pool).Create(ctx, dep); err != nil {
		t.Fatal(err)
	}
	digest := make([]byte, 32)
	var jobID int64
	err = f.pool.QueryRow(ctx, `INSERT INTO agent_jobs(schema_version,job_kind,owner_id,project_id,environment_id,server_id,deployment_id,repository_id,full_ref,commit_sha,priority,available_at,idempotency_key,payload_digest,state) VALUES (1,'deployment_execution',$1,'project',$2,$3,$4,201,'refs/heads/main','0123456789abcdef0123456789abcdef01234567',$5,`+availableAtExpr+`,$6,$7,'queued') RETURNING id`,
		f.ownerID, environmentID, serverID, deploymentID, priority, key, digest).Scan(&jobID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, `INSERT INTO agent_job_events(job_id,sequence,event_type) VALUES ($1,1,'enqueued')`, jobID); err != nil {
		t.Fatal(err)
	}
	return jobID
}

func (f leaseFixture) insertJob(t *testing.T, serverID, environmentID, key string, priority int16) int64 {
	t.Helper()
	return f.insertJobAt(t, serverID, environmentID, key, priority, "clock_timestamp()")
}

func TestAgentLeaseClaimSimultaneousOneWinnerPerJob(t *testing.T) {
	f := newLeaseFixture(t, 1)
	d := f.withCredential(t, 40)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	const goroutines = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	claims := make([][]int, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			leases, err := repo.Claim(ctx, f.serverID, d, 1)
			if err != nil {
				t.Errorf("claim %d: %v", i, err)
				return
			}
			for range leases {
				claims[i] = append(claims[i], 1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	total := 0
	for _, c := range claims {
		total += len(c)
	}
	if total != 1 {
		t.Fatalf("expected exactly one winner, got %d", total)
	}
	var state string
	var attemptCount int
	if err := f.pool.QueryRow(ctx, `SELECT state, attempt_count FROM agent_jobs WHERE id=$1`, f.jobID).Scan(&state, &attemptCount); err != nil {
		t.Fatal(err)
	}
	if state != "leased" || attemptCount != 1 {
		t.Fatalf("state=%s attemptCount=%d", state, attemptCount)
	}
}

func TestAgentLeaseClaimOneJobPerEnvironment(t *testing.T) {
	f := newLeaseFixture(t, 2)
	d := f.withCredential(t, 41)
	f.insertJob(t, f.serverID, f.envID, "second-same-env", 0)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	leases, err := repo.Claim(context.Background(), f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("expected exactly one lease for one environment, got %d", len(leases))
	}
}

func TestAgentLeaseClaimDistinctEnvironmentsBothClaimable(t *testing.T) {
	f := newLeaseFixture(t, 3)
	d := f.withCredential(t, 42)
	f.insertJob(t, f.serverID, "environment-2", "second-env", 0)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	leases, err := repo.Claim(context.Background(), f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 2 {
		t.Fatalf("expected two leases across two environments, got %d", len(leases))
	}
	seen := map[string]bool{}
	for _, l := range leases {
		seen[l.EnvironmentID] = true
	}
	if !seen[f.envID] || !seen["environment-2"] {
		t.Fatalf("leases=%+v", leases)
	}
}

func TestAgentLeaseClaimSkipsOtherServersJobs(t *testing.T) {
	f := newLeaseFixture(t, 4)
	d := f.withCredential(t, 43)
	f.insertServer(t, "server-other")
	f.insertJob(t, "server-other", "environment-other", "other-server-job", 0)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	leases, err := repo.Claim(context.Background(), f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].JobID != f.jobID {
		t.Fatalf("leases=%+v", leases)
	}
	var state string
	if err := f.pool.QueryRow(context.Background(), `SELECT state FROM agent_jobs WHERE server_id='server-other'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "queued" {
		t.Fatalf("other server job state=%s", state)
	}
}

func TestAgentLeaseClaimRevokedServerDenied(t *testing.T) {
	f := newLeaseFixture(t, 5)
	d := f.withCredential(t, 44)
	if _, err := f.pool.Exec(context.Background(), `UPDATE servers SET status='revoked', revoked_at=clock_timestamp() WHERE id=$1`, f.serverID); err != nil {
		t.Fatal(err)
	}
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	if _, err := repo.Claim(context.Background(), f.serverID, d, 5); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err=%v", err)
	}
	assertJobUntouched(t, f)
}

func TestAgentLeaseClaimRevokedCredentialDenied(t *testing.T) {
	f := newLeaseFixture(t, 6)
	d := f.withCredential(t, 45)
	if _, err := f.pool.Exec(context.Background(), `UPDATE server_credentials SET status='revoked', revoked_at=clock_timestamp() WHERE server_id=$1`, f.serverID); err != nil {
		t.Fatal(err)
	}
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	if _, err := repo.Claim(context.Background(), f.serverID, d, 5); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("err=%v", err)
	}
	assertJobUntouched(t, f)
}

func assertJobUntouched(t *testing.T, f leaseFixture) {
	t.Helper()
	var state string
	var attemptCount int
	if err := f.pool.QueryRow(context.Background(), `SELECT state, attempt_count FROM agent_jobs WHERE id=$1`, f.jobID).Scan(&state, &attemptCount); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || attemptCount != 0 {
		t.Fatalf("job mutated: state=%s attemptCount=%d", state, attemptCount)
	}
	var attempts int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_job_attempts WHERE job_id=$1`, f.jobID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Fatalf("unexpected attempt rows: %d", attempts)
	}
}

func TestAgentLeaseClaimRetryWaitFutureAvailableAtNotClaimable(t *testing.T) {
	f := newLeaseFixture(t, 7)
	d := f.withCredential(t, 46)
	// Move the fixture job straight into retry_wait with a future
	// available_at (a legal leased->retry_wait transition needs an attempt
	// first).
	attemptID := f.leaseJob(t, 1, 90)
	if _, err := f.pool.Exec(context.Background(), `UPDATE agent_job_attempts SET state='expired', closed_at=clock_timestamp() WHERE id=$1`, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE agent_jobs SET state='retry_wait', available_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, f.jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE environment_slots SET current_attempt_id=NULL WHERE environment_id=$1`, f.envID); err != nil {
		t.Fatal(err)
	}
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	leases, err := repo.Claim(context.Background(), f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("leases=%+v", leases)
	}
}

func TestAgentLeaseClaimReconciliationRequiredNotClaimable(t *testing.T) {
	f := newLeaseFixture(t, 8)
	d := f.withCredential(t, 47)
	attemptID := f.leaseJob(t, 1, 90)
	if _, err := f.pool.Exec(context.Background(), `UPDATE agent_job_attempts SET state='expired', closed_at=clock_timestamp() WHERE id=$1`, attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE agent_jobs SET state='retry_wait', available_at=clock_timestamp() WHERE id=$1`, f.jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE environment_slots SET reconciliation_required=true, reconciliation_attempt_id=$2 WHERE environment_id=$1`, f.envID, attemptID); err != nil {
		t.Fatal(err)
	}
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	leases, err := repo.Claim(context.Background(), f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("leases=%+v", leases)
	}
}

func TestAgentLeaseClaimFirstAttemptAssignsDeploymentSecondDoesNotRetransition(t *testing.T) {
	f := newLeaseFixture(t, 9)
	d := f.withCredential(t, 48)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	leases, err := repo.Claim(ctx, f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].AttemptNumber != 1 {
		t.Fatalf("leases=%+v", leases)
	}
	deploymentID := leases[0].DeploymentID
	dep, err := NewDeploymentRepository(f.pool).GetByID(ctx, deployment.ID(deploymentID))
	if err != nil {
		t.Fatal(err)
	}
	if dep.Status() != deployment.StatusAssigned {
		t.Fatalf("status after first claim=%s", dep.Status())
	}
	firstRevision := dep.Revision()

	// Simulate expiry/retry so a second attempt can be claimed.
	if _, err = f.pool.Exec(ctx, `UPDATE agent_job_attempts SET state='expired', closed_at=clock_timestamp() WHERE id=$1`, leases[0].AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE agent_jobs SET state='retry_wait', available_at=clock_timestamp() WHERE id=$1`, f.jobID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE environment_slots SET current_attempt_id=NULL WHERE environment_id=$1`, f.envID); err != nil {
		t.Fatal(err)
	}

	second, err := repo.Claim(ctx, f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].AttemptNumber != 2 {
		t.Fatalf("second=%+v", second)
	}
	dep2, err := NewDeploymentRepository(f.pool).GetByID(ctx, deployment.ID(deploymentID))
	if err != nil {
		t.Fatal(err)
	}
	if dep2.Status() != deployment.StatusAssigned || dep2.Revision() != firstRevision {
		t.Fatalf("deployment re-transitioned on second claim: status=%s revision=%d want revision=%d", dep2.Status(), dep2.Revision(), firstRevision)
	}
}

func TestAgentLeaseClaimDeadlinesFromPostgresTime(t *testing.T) {
	f := newLeaseFixture(t, 10)
	d := f.withCredential(t, 49)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	ctx := context.Background()

	var before time.Time
	if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	leases, err := repo.Claim(ctx, f.serverID, d, 5)
	if err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases=%+v", leases)
	}
	lease := leases[0]
	if lease.LeaseExpiresAt.Before(before.Add(90*time.Second)) || lease.LeaseExpiresAt.After(after.Add(90*time.Second)) {
		t.Fatalf("lease deadline %s not within [%s,%s]", lease.LeaseExpiresAt, before.Add(90*time.Second), after.Add(90*time.Second))
	}
	if !lease.AuthorityExpiresAt.Equal(lease.LeaseExpiresAt) {
		t.Fatalf("authority=%s lease=%s", lease.AuthorityExpiresAt, lease.LeaseExpiresAt)
	}
}

func TestAgentLeaseClaimInjectedFailureRollsBackEveryStage(t *testing.T) {
	stages := []string{"slot_fence", "job_leased", "attempt_insert", "slot_occupy", "deployment", "secret_freeze", "job_event"}
	for i, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			f := newLeaseFixture(t, byte(60+i))
			d := f.withCredential(t, byte(120+i))
			repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
			injected := errors.New("injected failure at " + stage)
			repo.failAfter = func(s string) error {
				if s == stage {
					return injected
				}
				return nil
			}
			_, err := repo.Claim(context.Background(), f.serverID, d, 5)
			if !errors.Is(err, injected) {
				t.Fatalf("stage=%s err=%v", stage, err)
			}
			assertJobUntouched(t, f)
			// The slot row itself is pre-created in its own committed
			// statement before the claim transaction begins (see the
			// deadlock-avoidance note on Claim), so it is expected to exist
			// after a rollback -- but every field the rolled-back
			// transaction would have written must be unchanged.
			var fence int64
			var currentAttemptID *int64
			var reconciling bool
			if err := f.pool.QueryRow(context.Background(), `SELECT fence, current_attempt_id, reconciliation_required FROM environment_slots WHERE environment_id=$1`, f.envID).Scan(&fence, &currentAttemptID, &reconciling); err != nil {
				t.Fatal(err)
			}
			if fence != 0 || currentAttemptID != nil || reconciling {
				t.Fatalf("stage=%s environment slot mutated despite rollback: fence=%d currentAttemptID=%v reconciling=%v", stage, fence, currentAttemptID, reconciling)
			}
			var events int
			if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_job_events WHERE job_id=$1`, f.jobID).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if events != 1 {
				t.Fatalf("stage=%s events=%d, want only the original enqueue event", stage, events)
			}
			var deploymentID string
			var deploymentStatus string
			if err := f.pool.QueryRow(context.Background(), `SELECT j.deployment_id, d.status FROM agent_jobs j JOIN deployments d ON d.id=j.deployment_id WHERE j.id=$1`, f.jobID).Scan(&deploymentID, &deploymentStatus); err != nil {
				t.Fatal(err)
			}
			if deploymentStatus != string(deployment.StatusQueued) {
				t.Fatalf("stage=%s deployment status=%s, want queued", stage, deploymentStatus)
			}
		})
	}
}

func TestAgentLeaseRenew(t *testing.T) {
	ctx := context.Background()

	t.Run("same sequence idempotent", func(t *testing.T) {
		f := newLeaseFixture(t, 11)
		d := f.withCredential(t, 50)
		attemptID := f.leaseJob(t, 1, 90)
		repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
		first, err := repo.Renew(ctx, f.serverID, d, attemptID, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		replay, err := repo.Renew(ctx, f.serverID, d, attemptID, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		if !replay.LeaseExpiresAt.Equal(first.LeaseExpiresAt) || !replay.AuthorityExpiresAt.Equal(first.AuthorityExpiresAt) {
			t.Fatalf("replay changed deadlines: first=%+v replay=%+v", first, replay)
		}
	})

	t.Run("sequence plus one extends", func(t *testing.T) {
		f := newLeaseFixture(t, 12)
		d := f.withCredential(t, 51)
		attemptID := f.leaseJob(t, 1, 90)
		var before time.Time
		if err := f.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
		result, err := repo.Renew(ctx, f.serverID, d, attemptID, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		if result.LeaseExpiresAt.Before(before.Add(90 * time.Second)) {
			t.Fatalf("lease not extended: %s before %s", result.LeaseExpiresAt, before.Add(90*time.Second))
		}
	})

	t.Run("gap rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 13)
		d := f.withCredential(t, 52)
		attemptID := f.leaseJob(t, 1, 90)
		repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
		if _, err := repo.Renew(ctx, f.serverID, d, attemptID, 1, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Renew(ctx, f.serverID, d, attemptID, 1, 3); !errors.Is(err, ErrAgentLeaseStale) {
			t.Fatalf("gap err=%v", err)
		}
	})

	t.Run("lower rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 14)
		d := f.withCredential(t, 53)
		attemptID := f.leaseJob(t, 1, 90)
		repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
		if _, err := repo.Renew(ctx, f.serverID, d, attemptID, 1, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Renew(ctx, f.serverID, d, attemptID, 1, 0); !errors.Is(err, ErrAgentLeaseStale) {
			t.Fatalf("lower err=%v", err)
		}
	})

	t.Run("wrong fence rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 15)
		d := f.withCredential(t, 54)
		attemptID := f.leaseJob(t, 1, 90)
		repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
		if _, err := repo.Renew(ctx, f.serverID, d, attemptID, 2, 1); !errors.Is(err, ErrAgentLeaseStale) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("other server rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 16)
		f.withCredential(t, 55)
		attemptID := f.leaseJob(t, 1, 90)
		f.insertServer(t, "server-other-renew")
		otherDigest := digest(56)
		if _, err := f.pool.Exec(ctx, `INSERT INTO server_credentials(server_id,version,digest,status) VALUES ('server-other-renew',1,$1,'active')`, otherDigest[:]); err != nil {
			t.Fatal(err)
		}
		repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
		if _, err := repo.Renew(ctx, "server-other-renew", otherDigest, attemptID, 1, 1); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("expired authority rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 17)
		d := f.withCredential(t, 57)
		attemptID := f.leaseJob(t, 1, -5)
		repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
		if _, err := repo.Renew(ctx, f.serverID, d, attemptID, 1, 1); !errors.Is(err, ErrAgentLeaseStale) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("revoked credential rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 18)
		d := f.withCredential(t, 58)
		attemptID := f.leaseJob(t, 1, 90)
		if _, err := f.pool.Exec(ctx, `UPDATE server_credentials SET status='revoked', revoked_at=clock_timestamp() WHERE server_id=$1`, f.serverID); err != nil {
			t.Fatal(err)
		}
		repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
		if _, err := repo.Renew(ctx, f.serverID, d, attemptID, 1, 1); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("err=%v", err)
		}
		var sequence int
		if err := f.pool.QueryRow(ctx, `SELECT renewal_sequence FROM agent_job_attempts WHERE id=$1`, attemptID).Scan(&sequence); err != nil {
			t.Fatal(err)
		}
		if sequence != 0 {
			t.Fatalf("renewal mutated despite revoked credential: sequence=%d", sequence)
		}
	})

	t.Run("cancel cap respected", func(t *testing.T) {
		f := newLeaseFixture(t, 19)
		d := f.withCredential(t, 59)
		attemptID := f.leaseJob(t, 1, 30)
		if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET cancel_requested_at=clock_timestamp() WHERE id=$1`, f.jobID); err != nil {
			t.Fatal(err)
		}
		var cancelDeadline time.Time
		if err := f.pool.QueryRow(ctx, `UPDATE agent_job_attempts SET cancel_deadline_at=clock_timestamp()+interval '35 seconds' WHERE id=$1 RETURNING cancel_deadline_at`, attemptID).Scan(&cancelDeadline); err != nil {
			t.Fatal(err)
		}
		repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
		result, err := repo.Renew(ctx, f.serverID, d, attemptID, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		if !result.LeaseExpiresAt.Equal(cancelDeadline) || !result.AuthorityExpiresAt.Equal(cancelDeadline) {
			t.Fatalf("cap not respected: result=%+v cancelDeadline=%s", result, cancelDeadline)
		}
		if !result.CancelRequested {
			t.Fatal("expected CancelRequested=true")
		}
	})
}

func TestAgentLeaseRenewInjectedFailureRollsBack(t *testing.T) {
	f := newLeaseFixture(t, 20)
	d := f.withCredential(t, 70)
	attemptID := f.leaseJob(t, 1, 90)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	injected := errors.New("injected renew failure")
	repo.failAfter = func(stage string) error {
		if stage == "attempt_renew" {
			return injected
		}
		return nil
	}
	if _, err := repo.Renew(context.Background(), f.serverID, d, attemptID, 1, 1); !errors.Is(err, injected) {
		t.Fatalf("err=%v", err)
	}
	var sequence int
	var leaseExpiresAt time.Time
	if err := f.pool.QueryRow(context.Background(), `SELECT renewal_sequence, lease_expires_at FROM agent_job_attempts WHERE id=$1`, attemptID).Scan(&sequence, &leaseExpiresAt); err != nil {
		t.Fatal(err)
	}
	if sequence != 0 {
		t.Fatalf("renewal_sequence mutated despite injected failure: %d", sequence)
	}
}

// TestAgentLeaseClaimOppositeOrderNewEnvironmentsNoDeadlock reproduces the
// scenario that motivated pre-creating every candidate's environment_slots
// row (in sorted order) before the claim transaction begins: two servers
// each have queued jobs against the same two brand-new environments, but in
// opposite priority order, so their non-locking candidate discovery lists
// name those environments in opposite order. Before the fix, each claim's
// own per-candidate ON CONFLICT DO NOTHING insert could hold one
// uncommitted row and wait on the other's, aborting with a Postgres
// deadlock error. Run with -race -count=20.
func TestAgentLeaseClaimOppositeOrderNewEnvironmentsNoDeadlock(t *testing.T) {
	f := newLeaseFixture(t, 90)
	ctx := context.Background()
	f.insertServer(t, "server-deadlock-x")
	f.insertServer(t, "server-deadlock-y")
	credX := digest(210)
	credY := digest(211)
	if _, err := f.pool.Exec(ctx, `INSERT INTO server_credentials(server_id,version,digest,status) VALUES ('server-deadlock-x',1,$1,'active')`, credX[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO server_credentials(server_id,version,digest,status) VALUES ('server-deadlock-y',1,$1,'active')`, credY[:]); err != nil {
		t.Fatal(err)
	}
	// server-deadlock-x discovers env-deadlock-a before env-deadlock-b
	// (higher priority first); server-deadlock-y discovers them in the
	// opposite order.
	f.insertJob(t, "server-deadlock-x", "env-deadlock-a", "x-a-"+t.Name(), 10)
	f.insertJob(t, "server-deadlock-x", "env-deadlock-b", "x-b-"+t.Name(), 0)
	f.insertJob(t, "server-deadlock-y", "env-deadlock-b", "y-b-"+t.Name(), 10)
	f.insertJob(t, "server-deadlock-y", "env-deadlock-a", "y-a-"+t.Name(), 0)

	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	start := make(chan struct{})
	var wg sync.WaitGroup
	var leasesX, leasesY []queue.ClaimedLease
	var errX, errY error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		leasesX, errX = repo.Claim(ctx, "server-deadlock-x", credX, 5)
	}()
	go func() {
		defer wg.Done()
		<-start
		leasesY, errY = repo.Claim(ctx, "server-deadlock-y", credY, 5)
	}()
	close(start)
	wg.Wait()

	if errX != nil {
		t.Fatalf("server-deadlock-x claim: %v", errX)
	}
	if errY != nil {
		t.Fatalf("server-deadlock-y claim: %v", errY)
	}
	perEnvironment := map[string]int{}
	for _, l := range append(append([]queue.ClaimedLease{}, leasesX...), leasesY...) {
		perEnvironment[l.EnvironmentID]++
	}
	for env, count := range perEnvironment {
		if count > 1 {
			t.Fatalf("environment %s claimed %d times", env, count)
		}
	}
	if len(perEnvironment) == 0 {
		t.Fatal("expected at least one environment claimed")
	}
}

// TestAgentLeaseRenewRejectsWhenSlotOccupiedByDifferentAttempt exercises the
// slot.current_attempt_id defense-in-depth check independently of the fence
// check: a second job in the same environment is leased at the same fence
// (contrived via direct SQL -- this cannot happen through Claim, which
// always bumps the fence on every new occupant) and takes over the slot's
// current_attempt_id, while the original attempt's own row is untouched and
// still reports itself "leased" with a matching fence. Renew must still
// reject it, and must not mutate it.
func TestAgentLeaseRenewRejectsWhenSlotOccupiedByDifferentAttempt(t *testing.T) {
	f := newLeaseFixture(t, 91)
	d := f.withCredential(t, 220)
	ctx := context.Background()
	attempt1 := f.leaseJob(t, 1, 90)

	shadowJob := f.insertJob(t, f.serverID, f.envID, "shadow-"+t.Name(), 0)
	if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='leased', attempt_count=attempt_count+1 WHERE id=$1`, shadowJob); err != nil {
		t.Fatal(err)
	}
	// A single WITH-evaluated deadline (not two separate clock_timestamp()
	// calls) so lease_expires_at and authority_expires_at can never diverge
	// by the few microseconds that would violate the table's
	// authority_expires_at<=lease_expires_at CHECK.
	var attempt2 int64
	if err := f.pool.QueryRow(ctx, `WITH t AS (SELECT clock_timestamp()+interval '90 seconds' AS deadline)
		INSERT INTO agent_job_attempts(job_id, attempt_number, server_id, environment_id, fence, state, lease_expires_at, authority_expires_at)
		SELECT $1,1,$2,$3,1,'leased',t.deadline,t.deadline FROM t RETURNING id`, shadowJob, f.serverID, f.envID).Scan(&attempt2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE environment_slots SET current_attempt_id=$2 WHERE environment_id=$1`, f.envID, attempt2); err != nil {
		t.Fatal(err)
	}

	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	if _, err := repo.Renew(ctx, f.serverID, d, attempt1, 1, 1); !errors.Is(err, ErrAgentLeaseStale) {
		t.Fatalf("err=%v", err)
	}
	var sequence int
	if err := f.pool.QueryRow(ctx, `SELECT renewal_sequence FROM agent_job_attempts WHERE id=$1`, attempt1).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	if sequence != 0 {
		t.Fatalf("attempt1 mutated despite slot occupied by a different attempt: sequence=%d", sequence)
	}
}

// assertNoClaimSideEffects checks that a denied Claim touched nothing: the
// fixture job is untouched (assertJobUntouched), no environment_slots row
// was created for its environment, and no event beyond the original enqueue
// exists.
func assertNoClaimSideEffects(t *testing.T, f leaseFixture) {
	t.Helper()
	ctx := context.Background()
	assertJobUntouched(t, f)
	var slots int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM environment_slots WHERE environment_id=$1`, f.envID).Scan(&slots); err != nil {
		t.Fatal(err)
	}
	if slots != 0 {
		t.Fatalf("environment slot created despite denied/no-op claim: %d", slots)
	}
	var events int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM agent_job_events WHERE job_id=$1`, f.jobID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("events=%d, want only the original enqueue event", events)
	}
}

// TestAgentLeaseClaimAuthenticatesBeforeAnyRead proves the cheap non-locking
// authentication check runs before candidate discovery or the
// environment_slots pre-create: an unknown server, and a real server with a
// wrong credential digest, must both be denied without creating any slot row
// or mutating the fixture's queued job/events, even though it has a runnable
// job the discovery query would otherwise find.
func TestAgentLeaseClaimAuthenticatesBeforeAnyRead(t *testing.T) {
	t.Run("unknown server", func(t *testing.T) {
		f := newLeaseFixture(t, 100)
		repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
		if _, err := repo.Claim(context.Background(), "unknown-server-id", digest(1), 5); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("err=%v", err)
		}
		assertNoClaimSideEffects(t, f)
	})
	t.Run("wrong digest for real server", func(t *testing.T) {
		f := newLeaseFixture(t, 101)
		f.withCredential(t, 230)
		repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
		if _, err := repo.Claim(context.Background(), f.serverID, digest(231), 5); !errors.Is(err, ErrServerAuthorizationDenied) {
			t.Fatalf("err=%v", err)
		}
		assertNoClaimSideEffects(t, f)
	})
}

// TestAgentLeaseClaimNonPositiveLimitNoop proves limit<=0 short-circuits
// before any validation or database access: even a malformed serverID and a
// zero credential digest return (nil, nil) rather than a denial, and the
// fixture's runnable job/events/slots are left completely untouched.
func TestAgentLeaseClaimNonPositiveLimitNoop(t *testing.T) {
	f := newLeaseFixture(t, 102)
	repo := NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))
	for _, limit := range []int{0, -1, -100} {
		leases, err := repo.Claim(context.Background(), "does-not-matter", [32]byte{}, limit)
		if err != nil || leases != nil {
			t.Fatalf("limit=%d leases=%v err=%v", limit, leases, err)
		}
	}
	assertNoClaimSideEffects(t, f)
}
