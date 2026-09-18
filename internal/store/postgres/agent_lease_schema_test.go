//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"autodeploy/internal/auth"

	"github.com/jackc/pgx/v5/pgxpool"
)

// leaseFixture bootstraps an owner (via freshGitHubRepository/freshAuthRepository),
// a verified push binding, a queued agent_jobs row with its 'enqueued' event
// produced through the real 000010 webhook-enqueue repository path, and a
// servers row scoped to the same owner for agent_job_attempts' server_id FK.
// It resets every lease-related table first so tests do not see each other's
// rows.
type leaseFixture struct {
	pool      *pgxpool.Pool
	ownerID   string
	principal auth.Principal
	jobID     int64
	serverID  string
	envID     string
}

func newLeaseFixture(t *testing.T, seed byte) leaseFixture {
	t.Helper()
	githubRepo, _, principal := webhookAuthority(t, "agent-lease-"+t.Name(), seed, seed+1)
	resetWebhookJobs(t, githubRepo.pool)
	ctx := context.Background()
	if _, err := githubRepo.pool.Exec(ctx, `INSERT INTO servers(id, owner_id, name, protocol_version, status) VALUES ('server', $1, 'server', 1, 'active')`, principal.OwnerID); err != nil {
		t.Fatal(err)
	}
	repo := NewWebhookJobRepository(githubRepo.pool, NewDeploymentRepository(githubRepo.pool))
	input := webhookInput(t, principal.OwnerID, "lease-delivery-"+t.Name(), seed, "lease-deployment-"+t.Name(), "lease-key-"+t.Name())
	result, err := repo.EnqueueVerifiedPush(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	f := leaseFixture{pool: githubRepo.pool, ownerID: principal.OwnerID, principal: principal, jobID: result.JobID, serverID: "server", envID: "environment"}
	f.insertRuntimeConfig(t, f.envID)
	return f
}

// insertRuntimeConfig ensures environmentID has an environment_runtime_configs
// row, with fixed, arbitrary-but-valid defaults (migration 000014's
// claim-time freeze fails a candidate whose environment has none). Callers
// that need a specific config value, or that want to exercise a
// concurrent administrator edit, use a dedicated environment id rather than
// mutating this shared default -- resetWebhookJobs truncates this table, but
// only at the *start* of the next newLeaseFixture/insertJobAt call, not
// between assertions within one test.
func (f leaseFixture) insertRuntimeConfig(t *testing.T, environmentID string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO environment_runtime_configs(environment_id, container_port, container_scheme, release_strategy, drain_seconds, cpu_millis, memory_bytes, restart_policy, health_policy_revision) VALUES ($1,8080,'http','stop_then_start',30,500,268435456,'unless-stopped',1) ON CONFLICT DO NOTHING`, environmentID); err != nil {
		t.Fatal(err)
	}
}

// insertSlot creates an environment_slots row for f.envID if one is not
// already present, so agent_job_attempts.environment_id can reference it.
func (f leaseFixture) insertSlot(t *testing.T) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO environment_slots(environment_id) VALUES ($1) ON CONFLICT DO NOTHING`, f.envID); err != nil {
		t.Fatal(err)
	}
}

// leaseJob transitions f's job from queued (or retry_wait) directly to
// leased with a fresh attempt row, mirroring the atomic claim boundary's
// job-side effect for test setup purposes (Step 3 owns the real claim
// transaction). It follows the same statement ordering the claim
// transaction must use so the agent_job_attempts fencing trigger accepts
// the row: (1) advance the environment slot's fence to the caller-chosen
// value, (2) transition the job into 'leased' with attempt_count
// incremented, (3) insert the attempt carrying that fence and the job's
// now-current attempt_count, (4) occupy the slot with that attempt's id --
// the same order Step 3's real Claim uses, so Renew's slot-occupant check
// sees a consistent fixture.
func (f leaseFixture) leaseJob(t *testing.T, fence int64, leaseSeconds int) int64 {
	t.Helper()
	f.insertSlot(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE environment_slots SET fence=$2 WHERE environment_id=$1`, f.envID, fence); err != nil {
		t.Fatal(err)
	}
	var attemptNumber int
	if err := f.pool.QueryRow(ctx, `UPDATE agent_jobs SET state='leased', attempt_count=attempt_count+1 WHERE id=$1 RETURNING attempt_count`, f.jobID).Scan(&attemptNumber); err != nil {
		t.Fatal(err)
	}
	var attemptID int64
	if err := f.pool.QueryRow(ctx, `WITH t AS (SELECT clock_timestamp() + make_interval(secs => $6) AS deadline)
		INSERT INTO agent_job_attempts(job_id, attempt_number, server_id, environment_id, fence, state, lease_expires_at, authority_expires_at)
		SELECT $1, $2, $3, $4, $5, 'leased', t.deadline, t.deadline FROM t RETURNING id`,
		f.jobID, attemptNumber, f.serverID, f.envID, fence, leaseSeconds).Scan(&attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE environment_slots SET current_attempt_id=$2 WHERE environment_id=$1`, f.envID, attemptID); err != nil {
		t.Fatal(err)
	}
	return attemptID
}

func TestAgentLeaseSchemaExistingWebhookJobRemainsValid(t *testing.T) {
	f := newLeaseFixture(t, 10)
	ctx := context.Background()

	var state string
	var attemptCount int
	var cancelRequestedAt *time.Time
	if err := f.pool.QueryRow(ctx, `SELECT state, attempt_count, cancel_requested_at FROM agent_jobs WHERE id=$1`, f.jobID).Scan(&state, &attemptCount, &cancelRequestedAt); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || attemptCount != 0 || cancelRequestedAt != nil {
		t.Fatalf("state=%s attemptCount=%d cancelRequestedAt=%v", state, attemptCount, cancelRequestedAt)
	}
	var eventType string
	var sequence int
	if err := f.pool.QueryRow(ctx, `SELECT event_type, sequence FROM agent_job_events WHERE job_id=$1`, f.jobID).Scan(&eventType, &sequence); err != nil {
		t.Fatal(err)
	}
	if eventType != "enqueued" || sequence != 1 {
		t.Fatalf("eventType=%s sequence=%d", eventType, sequence)
	}

	// The 000010 row must still accept the new legal queued->leased
	// transition under the replacement trigger.
	attemptID := f.leaseJob(t, 1, 90)
	if attemptID == 0 {
		t.Fatal("expected attempt id")
	}
	if err := f.pool.QueryRow(ctx, `SELECT state FROM agent_jobs WHERE id=$1`, f.jobID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "leased" {
		t.Fatalf("state=%s", state)
	}
}

func TestAgentJobsLegalTransitionsAccepted(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		run  func(t *testing.T, f leaseFixture)
	}{
		{"queued->leased", func(t *testing.T, f leaseFixture) { f.leaseJob(t, 1, 90) }},
		{"queued->cancelled", func(t *testing.T, f leaseFixture) {
			if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='cancelled' WHERE id=$1`, f.jobID); err != nil {
				t.Fatal(err)
			}
		}},
		{"leased->succeeded", func(t *testing.T, f leaseFixture) {
			f.leaseJob(t, 1, 90)
			if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='succeeded' WHERE id=$1`, f.jobID); err != nil {
				t.Fatal(err)
			}
		}},
		{"leased->failed", func(t *testing.T, f leaseFixture) {
			f.leaseJob(t, 1, 90)
			if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='failed' WHERE id=$1`, f.jobID); err != nil {
				t.Fatal(err)
			}
		}},
		{"leased->dead", func(t *testing.T, f leaseFixture) {
			f.leaseJob(t, 1, 90)
			if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='dead' WHERE id=$1`, f.jobID); err != nil {
				t.Fatal(err)
			}
		}},
		{"leased->cancelled", func(t *testing.T, f leaseFixture) {
			f.leaseJob(t, 1, 90)
			if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='cancelled' WHERE id=$1`, f.jobID); err != nil {
				t.Fatal(err)
			}
		}},
		{"leased->leased (cancel_requested_at only)", func(t *testing.T, f leaseFixture) {
			f.leaseJob(t, 1, 90)
			if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET cancel_requested_at=clock_timestamp() WHERE id=$1`, f.jobID); err != nil {
				t.Fatal(err)
			}
		}},
		{"leased->retry_wait->leased", func(t *testing.T, f leaseFixture) {
			attemptID := f.leaseJob(t, 1, 90)
			if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET state='expired', closed_at=clock_timestamp() WHERE id=$1`, attemptID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='retry_wait', available_at=available_at+interval '5 seconds' WHERE id=$1`, f.jobID); err != nil {
				t.Fatal(err)
			}
			f.insertSlot(t)
			if _, err := f.pool.Exec(ctx, `UPDATE environment_slots SET fence=2 WHERE environment_id=$1`, f.envID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='leased', attempt_count=attempt_count+1 WHERE id=$1`, f.jobID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(ctx, `WITH t AS (SELECT clock_timestamp()+interval '90 seconds' AS deadline)
				INSERT INTO agent_job_attempts(job_id, attempt_number, server_id, environment_id, fence, state, lease_expires_at, authority_expires_at)
				SELECT $1, 2, $2, $3, 2, 'leased', t.deadline, t.deadline FROM t`, f.jobID, f.serverID, f.envID); err != nil {
				t.Fatal(err)
			}
		}},
		{"retry_wait->cancelled", func(t *testing.T, f leaseFixture) {
			f.leaseJob(t, 1, 90)
			if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='retry_wait', available_at=available_at+interval '5 seconds' WHERE id=$1`, f.jobID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='cancelled' WHERE id=$1`, f.jobID); err != nil {
				t.Fatal(err)
			}
		}},
		{"retry_wait->dead", func(t *testing.T, f leaseFixture) {
			f.leaseJob(t, 1, 90)
			if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='retry_wait', available_at=available_at+interval '5 seconds' WHERE id=$1`, f.jobID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='dead' WHERE id=$1`, f.jobID); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLeaseFixture(t, byte(20+i*2))
			tc.run(t, f)
		})
	}
}

func TestAgentJobsIllegalTransitionsRejected(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(t *testing.T, f leaseFixture)
		query string
		args  []any
	}{
		{
			name: "terminal resurrection succeeded->queued",
			setup: func(t *testing.T, f leaseFixture) {
				f.leaseJob(t, 1, 90)
				if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='succeeded' WHERE id=$1`, f.jobID); err != nil {
					t.Fatal(err)
				}
			},
			query: `UPDATE agent_jobs SET state='queued' WHERE id=$1`,
		},
		{
			name: "terminal resurrection cancelled->leased",
			setup: func(t *testing.T, f leaseFixture) {
				if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='cancelled' WHERE id=$1`, f.jobID); err != nil {
					t.Fatal(err)
				}
			},
			query: `UPDATE agent_jobs SET state='leased', attempt_count=attempt_count+1 WHERE id=$1`,
		},
		{
			name:  "skip queued->succeeded",
			setup: func(t *testing.T, f leaseFixture) {},
			query: `UPDATE agent_jobs SET state='succeeded' WHERE id=$1`,
		},
		{
			name:  "skip queued->retry_wait",
			setup: func(t *testing.T, f leaseFixture) {},
			query: `UPDATE agent_jobs SET state='retry_wait', available_at=available_at+interval '5 seconds' WHERE id=$1`,
		},
		{
			name:  "identity change owner_id",
			setup: func(t *testing.T, f leaseFixture) {},
			query: `UPDATE agent_jobs SET owner_id='someone-else', state='leased', attempt_count=attempt_count+1 WHERE id=$1`,
		},
		{
			name: "attempt_count decrease",
			setup: func(t *testing.T, f leaseFixture) {
				f.leaseJob(t, 1, 90)
			},
			query: `UPDATE agent_jobs SET cancel_requested_at=clock_timestamp(), attempt_count=attempt_count-1 WHERE id=$1`,
		},
		{
			name:  "attempt_count skips ahead on lease",
			setup: func(t *testing.T, f leaseFixture) { f.insertSlot(t) },
			query: `UPDATE agent_jobs SET state='leased', attempt_count=attempt_count+2 WHERE id=$1`,
		},
		{
			name: "available_at changes outside retry_wait",
			setup: func(t *testing.T, f leaseFixture) {
				f.leaseJob(t, 1, 90)
			},
			query: `UPDATE agent_jobs SET available_at=available_at+interval '1 second' WHERE id=$1`,
		},
		{
			name: "cancel_requested_at reset once set",
			setup: func(t *testing.T, f leaseFixture) {
				f.leaseJob(t, 1, 90)
				if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET cancel_requested_at=clock_timestamp() WHERE id=$1`, f.jobID); err != nil {
					t.Fatal(err)
				}
			},
			query: `UPDATE agent_jobs SET cancel_requested_at=clock_timestamp()+interval '1 second' WHERE id=$1`,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLeaseFixture(t, byte(60+i*2))
			tc.setup(t, f)
			args := append([]any{f.jobID}, tc.args...)
			_, err := f.pool.Exec(ctx, tc.query, args...)
			if err == nil {
				t.Fatal("expected rejection")
			}
			assertPgSQLState(t, err, "P0001")
		})
	}
}

func TestAgentJobsDeleteRejected(t *testing.T) {
	f := newLeaseFixture(t, 100)
	_, err := f.pool.Exec(context.Background(), `DELETE FROM agent_jobs WHERE id=$1`, f.jobID)
	if err == nil {
		t.Fatal("expected rejection")
	}
	assertPgSQLState(t, err, "P0001")
}

func TestAgentJobAttemptsTransitionsAndConstraints(t *testing.T) {
	ctx := context.Background()

	t.Run("renewal advances sequence", func(t *testing.T) {
		f := newLeaseFixture(t, 110)
		attemptID := f.leaseJob(t, 1, 90)
		if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET renewal_sequence=1, lease_expires_at=t.deadline, authority_expires_at=t.deadline FROM (SELECT clock_timestamp()+interval '90 seconds' AS deadline) t WHERE id=$1`, attemptID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("renewal sequence decrease rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 112)
		attemptID := f.leaseJob(t, 1, 90)
		if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET renewal_sequence=1 WHERE id=$1`, attemptID); err != nil {
			t.Fatal(err)
		}
		_, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET renewal_sequence=0 WHERE id=$1`, attemptID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "P0001")
	})

	t.Run("closed attempt mutation rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 114)
		attemptID := f.leaseJob(t, 1, 90)
		if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET state='succeeded', closed_at=clock_timestamp(), result_operation_id='op-114', result_digest=decode(repeat('ab',32),'hex') WHERE id=$1`, attemptID); err != nil {
			t.Fatal(err)
		}
		_, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET renewal_sequence=1 WHERE id=$1`, attemptID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "P0001")
	})

	t.Run("attempt identity change rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 116)
		attemptID := f.leaseJob(t, 1, 90)
		_, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET fence=fence+1 WHERE id=$1`, attemptID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "P0001")
	})

	t.Run("two leased attempts for one job rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 118)
		// Leave the first attempt open (never closed) and legally advance
		// the job through another retry_wait->leased cycle to obtain a
		// second attempt_number/fence pair that satisfies the insert
		// trigger's per-row checks on its own; only the one-leased-per-job
		// partial unique index should then catch the resulting race.
		f.leaseJob(t, 1, 90)
		if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='retry_wait', available_at=available_at+interval '5 seconds' WHERE id=$1`, f.jobID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='leased', attempt_count=attempt_count+1 WHERE id=$1`, f.jobID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE environment_slots SET fence=2 WHERE environment_id=$1`, f.envID); err != nil {
			t.Fatal(err)
		}
		_, err := f.pool.Exec(ctx, `WITH t AS (SELECT clock_timestamp()+interval '90 seconds' AS deadline)
			INSERT INTO agent_job_attempts(job_id, attempt_number, server_id, environment_id, fence, state, lease_expires_at, authority_expires_at)
			SELECT $1, 2, $2, $3, 2, 'leased', t.deadline, t.deadline FROM t`, f.jobID, f.serverID, f.envID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "23505")
	})

	t.Run("authority exceeding lease rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 120)
		f.insertSlot(t)
		if _, err := f.pool.Exec(ctx, `UPDATE environment_slots SET fence=1 WHERE environment_id=$1`, f.envID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='leased', attempt_count=attempt_count+1 WHERE id=$1`, f.jobID); err != nil {
			t.Fatal(err)
		}
		_, err := f.pool.Exec(ctx, `INSERT INTO agent_job_attempts(job_id, attempt_number, server_id, environment_id, fence, state, lease_expires_at, authority_expires_at)
			VALUES ($1, 1, $2, $3, 1, 'leased', clock_timestamp()+interval '90 seconds', clock_timestamp()+interval '91 seconds')`, f.jobID, f.serverID, f.envID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "23514")
	})

	t.Run("result digest without operation id rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 122)
		attemptID := f.leaseJob(t, 1, 90)
		_, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET state='succeeded', closed_at=clock_timestamp(), result_digest=decode(repeat('ab',32),'hex') WHERE id=$1`, attemptID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "23514")
	})

	t.Run("result operation id and digest set together and immutable", func(t *testing.T) {
		f := newLeaseFixture(t, 124)
		attemptID := f.leaseJob(t, 1, 90)
		if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET state='succeeded', closed_at=clock_timestamp(), result_operation_id='op-1', result_digest=decode(repeat('ab',32),'hex') WHERE id=$1`, attemptID); err != nil {
			t.Fatal(err)
		}
		_, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET result_operation_id='op-2' WHERE id=$1`, attemptID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "P0001")
	})

	t.Run("delete rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 126)
		attemptID := f.leaseJob(t, 1, 90)
		_, err := f.pool.Exec(ctx, `DELETE FROM agent_job_attempts WHERE id=$1`, attemptID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "P0001")
	})
}

// TestAgentJobAttemptClosureShapesAccepted exercises every legal
// state/failure_class/result shape from the ADR 0004 closure CHECK: leased
// (nothing set), succeeded (result only), failed (failure_class and
// result), expired (nothing set, even though it consumed an attempt), and
// cancelled either with a result (agent cancellation acknowledgement) or
// without one (closed by the expiry sweeper before any ack arrived). Each
// case reads the row back and asserts the persisted shape so the test
// cannot pass vacuously on a no-op write.
func TestAgentJobAttemptClosureShapesAccepted(t *testing.T) {
	ctx := context.Background()

	assertShape := func(t *testing.T, f leaseFixture, attemptID int64, wantState string, wantFailureClass *string, wantResult bool) {
		t.Helper()
		var state string
		var failureClass *string
		var resultOperationID *string
		var resultDigest []byte
		if err := f.pool.QueryRow(ctx, `SELECT state, failure_class, result_operation_id, result_digest FROM agent_job_attempts WHERE id=$1`, attemptID).
			Scan(&state, &failureClass, &resultOperationID, &resultDigest); err != nil {
			t.Fatal(err)
		}
		if state != wantState {
			t.Fatalf("state=%s want=%s", state, wantState)
		}
		gotClass := failureClass != nil
		wantClass := wantFailureClass != nil
		if gotClass != wantClass || (gotClass && *failureClass != *wantFailureClass) {
			t.Fatalf("failure_class=%v want=%v", failureClass, wantFailureClass)
		}
		if (resultOperationID != nil) != wantResult || (resultDigest != nil) != wantResult {
			t.Fatalf("result_operation_id=%v result_digest=%v want present=%v", resultOperationID, resultDigest, wantResult)
		}
	}

	t.Run("leased carries neither failure_class nor result", func(t *testing.T) {
		f := newLeaseFixture(t, 180)
		attemptID := f.leaseJob(t, 1, 90)
		assertShape(t, f, attemptID, "leased", nil, false)
	})

	t.Run("succeeded carries result and no failure_class", func(t *testing.T) {
		f := newLeaseFixture(t, 182)
		attemptID := f.leaseJob(t, 1, 90)
		if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET state='succeeded', closed_at=clock_timestamp(), result_operation_id='op-182', result_digest=decode(repeat('ab',32),'hex') WHERE id=$1`, attemptID); err != nil {
			t.Fatal(err)
		}
		assertShape(t, f, attemptID, "succeeded", nil, true)
	})

	t.Run("failed carries failure_class and result", func(t *testing.T) {
		f := newLeaseFixture(t, 184)
		attemptID := f.leaseJob(t, 1, 90)
		if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET state='failed', closed_at=clock_timestamp(), failure_class='transient', result_operation_id='op-184', result_digest=decode(repeat('ab',32),'hex') WHERE id=$1`, attemptID); err != nil {
			t.Fatal(err)
		}
		transient := "transient"
		assertShape(t, f, attemptID, "failed", &transient, true)
	})

	t.Run("expired carries neither failure_class nor result", func(t *testing.T) {
		f := newLeaseFixture(t, 186)
		attemptID := f.leaseJob(t, 1, 90)
		if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET state='expired', closed_at=clock_timestamp() WHERE id=$1`, attemptID); err != nil {
			t.Fatal(err)
		}
		assertShape(t, f, attemptID, "expired", nil, false)
	})

	t.Run("cancelled with agent acknowledgement result accepted", func(t *testing.T) {
		f := newLeaseFixture(t, 188)
		attemptID := f.leaseJob(t, 1, 90)
		if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET cancel_requested_at=clock_timestamp() WHERE id=$1`, f.jobID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET state='cancelled', closed_at=clock_timestamp(), result_operation_id='op-188', result_digest=decode(repeat('ab',32),'hex') WHERE id=$1`, attemptID); err != nil {
			t.Fatal(err)
		}
		assertShape(t, f, attemptID, "cancelled", nil, true)
	})

	t.Run("cancelled without result (closed by expiry sweeper) accepted", func(t *testing.T) {
		f := newLeaseFixture(t, 190)
		attemptID := f.leaseJob(t, 1, 90)
		if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET state='cancelled', closed_at=clock_timestamp() WHERE id=$1`, attemptID); err != nil {
			t.Fatal(err)
		}
		assertShape(t, f, attemptID, "cancelled", nil, false)
	})
}

// TestAgentJobAttemptClosureShapesRejected asserts the closure CHECK rejects
// every state/failure_class/result combination ADR 0004 forbids.
func TestAgentJobAttemptClosureShapesRejected(t *testing.T) {
	ctx := context.Background()

	t.Run("succeeded with failure_class rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 192)
		attemptID := f.leaseJob(t, 1, 90)
		_, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET state='succeeded', closed_at=clock_timestamp(), failure_class='transient', result_operation_id='op-192', result_digest=decode(repeat('ab',32),'hex') WHERE id=$1`, attemptID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "23514")
	})

	t.Run("expired with result rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 194)
		attemptID := f.leaseJob(t, 1, 90)
		_, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET state='expired', closed_at=clock_timestamp(), result_operation_id='op-194', result_digest=decode(repeat('ab',32),'hex') WHERE id=$1`, attemptID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "23514")
	})

	t.Run("failed without failure_class rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 196)
		attemptID := f.leaseJob(t, 1, 90)
		_, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET state='failed', closed_at=clock_timestamp(), result_operation_id='op-196', result_digest=decode(repeat('ab',32),'hex') WHERE id=$1`, attemptID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "23514")
	})
}

// TestAgentJobAttemptFencingRejected exercises the BEFORE INSERT trigger
// that ties a new attempt to its environment slot's current fence and its
// job's current server/environment/attempt_count (ADR 0004 atomic claim
// boundary + lock hierarchy: slot, then job, then attempt). Each case
// otherwise mirrors leaseJob's real ordering (slot fence advanced, job
// transitioned into leased) but deliberately breaks exactly one of the
// three cross-row predicates the trigger enforces.
func TestAgentJobAttemptFencingRejected(t *testing.T) {
	ctx := context.Background()

	t.Run("attempt fence not matching slot fence rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 200)
		f.insertSlot(t)
		// Slot fence stays at its default 0; the job is leased (so
		// attempt_number=1 is otherwise legal) but the attempt claims a
		// fence the slot never advanced to.
		if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='leased', attempt_count=attempt_count+1 WHERE id=$1`, f.jobID); err != nil {
			t.Fatal(err)
		}
		_, err := f.pool.Exec(ctx, `INSERT INTO agent_job_attempts(job_id, attempt_number, server_id, environment_id, fence, state, lease_expires_at, authority_expires_at)
			VALUES ($1, 1, $2, $3, 1, 'leased', clock_timestamp()+interval '90 seconds', clock_timestamp()+interval '90 seconds')`, f.jobID, f.serverID, f.envID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "P0001")
	})

	t.Run("attempt server_id not matching job server_id rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 202)
		f.insertSlot(t)
		if _, err := f.pool.Exec(ctx, `INSERT INTO servers(id, owner_id, name, protocol_version, status) VALUES ('server-202', $1, 'server-202', 1, 'active')`, f.ownerID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE environment_slots SET fence=1 WHERE environment_id=$1`, f.envID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='leased', attempt_count=attempt_count+1 WHERE id=$1`, f.jobID); err != nil {
			t.Fatal(err)
		}
		_, err := f.pool.Exec(ctx, `INSERT INTO agent_job_attempts(job_id, attempt_number, server_id, environment_id, fence, state, lease_expires_at, authority_expires_at)
			VALUES ($1, 1, 'server-202', $2, 1, 'leased', clock_timestamp()+interval '90 seconds', clock_timestamp()+interval '90 seconds')`, f.jobID, f.envID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "P0001")
	})

	t.Run("attempt_number not matching job attempt_count rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 204)
		f.insertSlot(t)
		if _, err := f.pool.Exec(ctx, `UPDATE environment_slots SET fence=1 WHERE environment_id=$1`, f.envID); err != nil {
			t.Fatal(err)
		}
		// Job attempt_count becomes 1, but the attempt claims number 2.
		if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET state='leased', attempt_count=attempt_count+1 WHERE id=$1`, f.jobID); err != nil {
			t.Fatal(err)
		}
		_, err := f.pool.Exec(ctx, `INSERT INTO agent_job_attempts(job_id, attempt_number, server_id, environment_id, fence, state, lease_expires_at, authority_expires_at)
			VALUES ($1, 2, $2, $3, 1, 'leased', clock_timestamp()+interval '90 seconds', clock_timestamp()+interval '90 seconds')`, f.jobID, f.serverID, f.envID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "P0001")
	})
}

// TestAgentJobAttemptCancelDeadline exercises the update trigger's
// cancel_deadline_at rules (ADR 0004 cancellation step 1): it may only be
// set on a leased attempt whose job has recorded cancel_requested_at, and
// once set it is immutable.
func TestAgentJobAttemptCancelDeadline(t *testing.T) {
	ctx := context.Background()

	t.Run("cancel_deadline_at without job cancel_requested_at rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 210)
		attemptID := f.leaseJob(t, 1, 90)
		_, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET cancel_deadline_at=clock_timestamp()+interval '60 seconds', authority_expires_at=clock_timestamp()+interval '60 seconds' WHERE id=$1`, attemptID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "P0001")
	})

	t.Run("cancel_deadline_at moved later once set rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 212)
		attemptID := f.leaseJob(t, 1, 90)
		if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET cancel_requested_at=clock_timestamp() WHERE id=$1`, f.jobID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET cancel_deadline_at=clock_timestamp()+interval '60 seconds', authority_expires_at=clock_timestamp()+interval '60 seconds' WHERE id=$1`, attemptID); err != nil {
			t.Fatal(err)
		}
		_, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET cancel_deadline_at=cancel_deadline_at+interval '1 second' WHERE id=$1`, attemptID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "P0001")
	})

	t.Run("cancel_deadline_at set after cancel_requested_at caps authority", func(t *testing.T) {
		f := newLeaseFixture(t, 214)
		attemptID := f.leaseJob(t, 1, 90)
		if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET cancel_requested_at=clock_timestamp() WHERE id=$1`, f.jobID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET cancel_deadline_at=t.deadline, lease_expires_at=t.deadline, authority_expires_at=t.deadline FROM (SELECT clock_timestamp()+interval '60 seconds' AS deadline) t WHERE id=$1`, attemptID); err != nil {
			t.Fatal(err)
		}
		var cancelDeadline, authorityExpires time.Time
		if err := f.pool.QueryRow(ctx, `SELECT cancel_deadline_at, authority_expires_at FROM agent_job_attempts WHERE id=$1`, attemptID).Scan(&cancelDeadline, &authorityExpires); err != nil {
			t.Fatal(err)
		}
		if !authorityExpires.Equal(cancelDeadline) {
			t.Fatalf("authority_expires_at=%v want equal to cancel_deadline_at=%v", authorityExpires, cancelDeadline)
		}
	})

	t.Run("authority_expires_at exceeding cancel_deadline_at rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 216)
		attemptID := f.leaseJob(t, 1, 90)
		if _, err := f.pool.Exec(ctx, `UPDATE agent_jobs SET cancel_requested_at=clock_timestamp() WHERE id=$1`, f.jobID); err != nil {
			t.Fatal(err)
		}
		_, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET cancel_deadline_at=t.deadline, authority_expires_at=t.deadline+interval '1 second' FROM (SELECT clock_timestamp()+interval '60 seconds' AS deadline) t WHERE id=$1`, attemptID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "23514")
	})
}

func TestEnvironmentSlotFenceAndReconciliation(t *testing.T) {
	ctx := context.Background()

	t.Run("fence advances", func(t *testing.T) {
		f := newLeaseFixture(t, 130)
		f.insertSlot(t)
		if _, err := f.pool.Exec(ctx, `UPDATE environment_slots SET fence=fence+1 WHERE environment_id=$1`, f.envID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("fence decrease rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 132)
		f.insertSlot(t)
		if _, err := f.pool.Exec(ctx, `UPDATE environment_slots SET fence=5 WHERE environment_id=$1`, f.envID); err != nil {
			t.Fatal(err)
		}
		_, err := f.pool.Exec(ctx, `UPDATE environment_slots SET fence=4 WHERE environment_id=$1`, f.envID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "P0001")
	})

	t.Run("reconciliation flag requires attempt reference", func(t *testing.T) {
		f := newLeaseFixture(t, 134)
		f.insertSlot(t)
		_, err := f.pool.Exec(ctx, `UPDATE environment_slots SET reconciliation_required=true WHERE environment_id=$1`, f.envID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "23514")
	})

	t.Run("reconciliation flag with attempt reference accepted", func(t *testing.T) {
		f := newLeaseFixture(t, 136)
		attemptID := f.leaseJob(t, 1, 90)
		if _, err := f.pool.Exec(ctx, `UPDATE agent_job_attempts SET state='expired', closed_at=clock_timestamp() WHERE id=$1`, attemptID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE environment_slots SET fence=fence+1, reconciliation_required=true, reconciliation_attempt_id=$2 WHERE environment_id=$1`, f.envID, attemptID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("identity change rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 138)
		f.insertSlot(t)
		_, err := f.pool.Exec(ctx, `UPDATE environment_slots SET environment_id='other' WHERE environment_id=$1`, f.envID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "P0001")
	})

	t.Run("delete rejected", func(t *testing.T) {
		f := newLeaseFixture(t, 140)
		f.insertSlot(t)
		_, err := f.pool.Exec(ctx, `DELETE FROM environment_slots WHERE environment_id=$1`, f.envID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "P0001")
	})
}

func TestAgentJobEventsVocabularyAndSequence(t *testing.T) {
	ctx := context.Background()
	f := newLeaseFixture(t, 150)
	attemptID := f.leaseJob(t, 1, 90)

	for i, eventType := range []string{"claimed", "cancel_requested", "expired", "retry_scheduled", "succeeded", "failed", "dead", "reconciled"} {
		sequence := i + 2
		if _, err := f.pool.Exec(ctx, `INSERT INTO agent_job_events(job_id, sequence, event_type, attempt_id) VALUES ($1,$2,$3,$4)`, f.jobID, sequence, eventType, attemptID); err != nil {
			t.Fatalf("event_type=%s err=%v", eventType, err)
		}
	}

	t.Run("cancelled event without attempt accepted", func(t *testing.T) {
		if _, err := f.pool.Exec(ctx, `INSERT INTO agent_job_events(job_id, sequence, event_type) VALUES ($1,10,'cancelled')`, f.jobID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("duplicate sequence rejected", func(t *testing.T) {
		_, err := f.pool.Exec(ctx, `INSERT INTO agent_job_events(job_id, sequence, event_type) VALUES ($1,10,'cancelled')`, f.jobID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "23505")
	})

	t.Run("sequence below one rejected", func(t *testing.T) {
		_, err := f.pool.Exec(ctx, `INSERT INTO agent_job_events(job_id, sequence, event_type) VALUES ($1,0,'cancelled')`, f.jobID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "23514")
	})

	t.Run("unknown event type rejected", func(t *testing.T) {
		_, err := f.pool.Exec(ctx, `INSERT INTO agent_job_events(job_id, sequence, event_type) VALUES ($1,11,'unknown')`, f.jobID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "23514")
	})

	t.Run("update rejected", func(t *testing.T) {
		_, err := f.pool.Exec(ctx, `UPDATE agent_job_events SET event_type='claimed' WHERE job_id=$1 AND sequence=1`, f.jobID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "P0001")
	})

	t.Run("delete rejected", func(t *testing.T) {
		_, err := f.pool.Exec(ctx, `DELETE FROM agent_job_events WHERE job_id=$1 AND sequence=1`, f.jobID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "P0001")
	})
}

func TestFrozenSecretVersionsAppendOnly(t *testing.T) {
	ctx := context.Background()
	f := newLeaseFixture(t, 160)
	attemptID := f.leaseJob(t, 1, 90)

	if _, err := f.pool.Exec(ctx, `INSERT INTO secret_heads(id, owner_id, project_id, environment_id, purpose, delivery_scope, revision) VALUES ('secret-1',$1,'project','environment','build','build',0)`, f.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO secret_versions(id, secret_id, owner_id, project_id, environment_id, version_number, state, suite, aad_version, kek_id, ciphertext, wrapped_dek)
		VALUES ('secret-version-1','secret-1',$1,'project','environment',1,'active','AES-256-GCM-RANDOMNONCE-v1','v1','kek-1', decode(repeat('ab',30),'hex'), decode(repeat('cd',60),'hex'))`, f.ownerID); err != nil {
		t.Fatal(err)
	}

	if _, err := f.pool.Exec(ctx, `INSERT INTO frozen_secret_versions(attempt_id, secret_version_id, variable_name, delivery_scope) VALUES ($1,'secret-version-1','BUILD_TOKEN','build')`, attemptID); err != nil {
		t.Fatal(err)
	}

	t.Run("update rejected", func(t *testing.T) {
		_, err := f.pool.Exec(ctx, `UPDATE frozen_secret_versions SET secret_version_id='secret-version-1' WHERE attempt_id=$1`, attemptID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "P0001")
	})

	t.Run("delete rejected", func(t *testing.T) {
		_, err := f.pool.Exec(ctx, `DELETE FROM frozen_secret_versions WHERE attempt_id=$1`, attemptID)
		if err == nil {
			t.Fatal("expected rejection")
		}
		assertPgSQLState(t, err, "P0001")
	})
}

// indexColumn is one key column of an index, in declared order.
type indexColumn struct {
	name string
	desc bool
}

// assertIndexColumns asserts an index's exact key column names, order, and
// ASC/DESC direction. pg_get_indexdef(oid, colno, pretty) gives the column
// expression at each 1-based position without its sort option, so direction
// is read separately from pg_index.indoption (bit 0 of each entry is DESC);
// together these pin down composition and order exactly, independent of
// how a given PostgreSQL version renders the whole CREATE INDEX string.
func assertIndexColumns(t *testing.T, pool *pgxpool.Pool, indexName string, want []indexColumn) {
	t.Helper()
	ctx := context.Background()
	var indoption []int32
	if err := pool.QueryRow(ctx, `SELECT ARRAY(SELECT x::int FROM unnest(indoption::int2[]) AS x) FROM pg_index WHERE indexrelid = $1::regclass`, indexName).Scan(&indoption); err != nil {
		t.Fatalf("index %s: %v", indexName, err)
	}
	if len(indoption) != len(want) {
		t.Fatalf("index %s: got %d key columns, want %d", indexName, len(indoption), len(want))
	}
	for i, w := range want {
		var colName string
		if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef($1::regclass, $2, true)`, indexName, i+1).Scan(&colName); err != nil {
			t.Fatalf("index %s column %d: %v", indexName, i+1, err)
		}
		if colName != w.name {
			t.Fatalf("index %s column %d: got %q, want %q", indexName, i+1, colName, w.name)
		}
		if gotDesc := indoption[i]&1 != 0; gotDesc != w.desc {
			t.Fatalf("index %s column %d (%s): got desc=%v, want desc=%v", indexName, i+1, colName, gotDesc, w.desc)
		}
	}
}

// assertIndexPredicate asserts an index's exact partial-index predicate, as
// PostgreSQL canonicalizes it (e.g. `state IN (...)` reads back as `state =
// ANY (ARRAY[...])`).
func assertIndexPredicate(t *testing.T, pool *pgxpool.Pool, indexName, want string) {
	t.Helper()
	var pred string
	if err := pool.QueryRow(context.Background(), `SELECT pg_get_expr(indpred, indrelid) FROM pg_index WHERE indexrelid = $1::regclass`, indexName).Scan(&pred); err != nil {
		t.Fatalf("index %s: %v", indexName, err)
	}
	if pred != want {
		t.Fatalf("index %s predicate: got %q, want %q", indexName, pred, want)
	}
}

func TestAgentLeaseIndexesExist(t *testing.T) {
	f := newLeaseFixture(t, 170)

	assertIndexColumns(t, f.pool, "agent_jobs_runnable_idx", []indexColumn{
		{name: "server_id"},
		{name: "priority", desc: true},
		{name: "available_at"},
		{name: "created_at"},
		{name: "id"},
	})
	assertIndexPredicate(t, f.pool, "agent_jobs_runnable_idx", `(state = ANY (ARRAY['queued'::text, 'retry_wait'::text]))`)

	assertIndexColumns(t, f.pool, "agent_job_attempts_one_leased_uq", []indexColumn{{name: "job_id"}})
	assertIndexPredicate(t, f.pool, "agent_job_attempts_one_leased_uq", `(state = 'leased'::text)`)

	assertIndexColumns(t, f.pool, "agent_job_attempts_authority_expiry_idx", []indexColumn{
		{name: "authority_expires_at"},
		{name: "id"},
	})
	assertIndexPredicate(t, f.pool, "agent_job_attempts_authority_expiry_idx", `(state = 'leased'::text)`)
}
