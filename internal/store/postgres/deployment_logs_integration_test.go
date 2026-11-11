//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type logFixture struct {
	opsFixture
	logs      *DeploymentLogRepository
	digest    [32]byte
	attemptID int64
	fence     int64
}

// newLogFixture builds the ops fixture (owners A and B), a credential for the
// fixture server and one leased attempt (fence 1) of owner A's job.
func newLogFixture(t *testing.T, seed byte) logFixture {
	t.Helper()
	o := newOpsFixture(t, seed)
	d := o.withCredential(t, seed+7)
	attemptID := o.leaseJob(t, 1, 600)
	return logFixture{opsFixture: o, logs: NewDeploymentLogRepository(o.pool), digest: d, attemptID: attemptID, fence: 1}
}

func (f logFixture) append(ctx context.Context, epoch int64, chunks ...LogChunkInput) (AppendLogResult, error) {
	return f.logs.AppendLogChunks(ctx, f.serverID, f.digest, f.attemptID, f.fence, epoch, chunks)
}

func (f logFixture) deploymentID(t *testing.T) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), `SELECT deployment_id FROM agent_jobs WHERE id=$1`, f.jobID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f logFixture) chunkCount(t *testing.T, attemptID int64) (rows int, bytes int64, markers int) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*), COALESCE(sum(octet_length(body)),0), count(*) FILTER (WHERE stream='system' AND writer_epoch=0 AND seq=1) FROM deployment_log_chunks WHERE attempt_id=$1`, attemptID).Scan(&rows, &bytes, &markers); err != nil {
		t.Fatal(err)
	}
	return
}

func lines(stream string, from, n int, body string) []LogChunkInput {
	out := make([]LogChunkInput, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, LogChunkInput{Stream: stream, Seq: int64(from + i), Body: body})
	}
	return out
}

// asReplica runs fn in a transaction with triggers disabled so a test can
// place attempts in states/times the lease trigger hierarchy would not allow
// directly (e.g. closed 40 minutes ago). Test-only.
func asReplica(t *testing.T, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, fn func(tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	fn(tx)
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func (f logFixture) closeAttempt(t *testing.T, attemptID int64, ago string) {
	t.Helper()
	asReplica(t, f.pool, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `UPDATE agent_job_attempts SET state='failed', failure_class='transient', result_operation_id='op', result_digest=decode(repeat('00',32),'hex'), closed_at=clock_timestamp()-$2::interval WHERE id=$1`, attemptID, ago); err != nil {
			t.Fatal(err)
		}
	})
}

func TestDeploymentLogsListOwnerIsolationAndPaging(t *testing.T) {
	f := newLogFixture(t, 170)
	ctx := context.Background()
	if res, err := f.append(ctx, 1, lines("build", 1, 5, "line")...); err != nil || res.Accepted != 5 {
		t.Fatalf("append = %+v, %v", res, err)
	}
	// A second attempt of the same job (attempt 2) contributes rows after.
	f.closeAttempt(t, f.attemptID, "1 minute")
	var a2 int64
	asReplica(t, f.pool, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `WITH t AS (SELECT clock_timestamp()+interval '1 hour' AS d)
			INSERT INTO agent_job_attempts(job_id, attempt_number, server_id, environment_id, fence, state, lease_expires_at, authority_expires_at)
			SELECT $1,2,$2,$3,2,'leased',t.d,t.d FROM t RETURNING id`, f.jobID, f.serverID, f.envID).Scan(&a2); err != nil {
			t.Fatal(err)
		}
	})
	if res, err := f.logs.AppendLogChunks(ctx, f.serverID, f.digest, a2, 2, 9, []LogChunkInput{{Stream: "phase", Seq: 1, Body: "second attempt"}}); err != nil || res.Accepted != 1 {
		t.Fatalf("append a2 = %+v, %v", res, err)
	}

	dep := f.deploymentID(t)
	page, err := f.logs.ListDeploymentLogs(ctx, f.principal, dep, 0, 0)
	if err != nil || len(page.Items) != 6 || page.Next != nil {
		t.Fatalf("owner A list = %d items next=%v err=%v", len(page.Items), page.Next, err)
	}
	for i := 1; i < len(page.Items); i++ {
		if page.Items[i].ID <= page.Items[i-1].ID {
			t.Fatalf("not oldest-first: %+v", page.Items)
		}
	}
	if page.Items[0].AttemptNumber != 1 || page.Items[5].AttemptNumber != 2 || page.Items[5].Body != "second attempt" || page.Items[5].Stream != "phase" {
		t.Fatalf("attempt numbers/rows wrong: %+v", page.Items)
	}

	// Keyset paging: limit 4 -> Next set; following page picks up the rest.
	p1, err := f.logs.ListDeploymentLogs(ctx, f.principal, dep, 0, 4)
	if err != nil || len(p1.Items) != 4 || p1.Next == nil || *p1.Next != p1.Items[3].ID {
		t.Fatalf("page1 = %d next=%v err=%v", len(p1.Items), p1.Next, err)
	}
	p2, err := f.logs.ListDeploymentLogs(ctx, f.principal, dep, *p1.Next, 4)
	if err != nil || len(p2.Items) != 2 || p2.Next != nil || p2.Items[0].ID != page.Items[4].ID {
		t.Fatalf("page2 = %+v next=%v err=%v", p2.Items, p2.Next, err)
	}

	// Owner B (same project/environment ids) must not see owner A's logs, and
	// cannot tell a foreign id from an unknown one.
	if _, err = f.logs.ListDeploymentLogs(ctx, f.principalB, dep, 0, 0); !errors.Is(err, ErrOperationsNotFound) {
		t.Fatalf("foreign owner err = %v", err)
	}
	if _, err = f.logs.ListDeploymentLogs(ctx, f.principal, "no-such-deployment", 0, 0); !errors.Is(err, ErrOperationsNotFound) {
		t.Fatalf("unknown err = %v", err)
	}
	if _, err = f.logs.ListDeploymentLogs(ctx, f.principal, "", 0, 0); !errors.Is(err, ErrOperationsNotFound) {
		t.Fatalf("empty id err = %v", err)
	}
	// Owner B's own deployment with no chunks is an empty page, never A's rows.
	seedOpsDeployment(t, f.pool, opsOwnerB, "project", "environment", "b-dep", time.Now())
	if p, err := f.logs.ListDeploymentLogs(ctx, f.principalB, "b-dep", 0, 0); err != nil || len(p.Items) != 0 {
		t.Fatalf("owner B own = %+v, %v", p.Items, err)
	}
	for _, bad := range []struct {
		after int64
		limit int
	}{{-1, 10}, {0, -1}, {0, MaxLogPageLimit + 1}} {
		if _, err = f.logs.ListDeploymentLogs(ctx, f.principal, dep, bad.after, bad.limit); !errors.Is(err, ErrOperationsInvalidInput) {
			t.Fatalf("bad paging %+v err = %v", bad, err)
		}
	}
	// A revoked session is denied before anything is read.
	if _, err = f.pool.Exec(ctx, `UPDATE admin_sessions SET revoked_at=clock_timestamp(), revoked_reason='logout' WHERE id=$1`, f.principal.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.logs.ListDeploymentLogs(ctx, f.principal, dep, 0, 0); !errors.Is(err, ErrServerAuthorizationDenied) {
		t.Fatalf("revoked session err = %v", err)
	}
}

func TestDeploymentLogsAppendIdempotentReplay(t *testing.T) {
	f := newLogFixture(t, 171)
	ctx := context.Background()
	batch := []LogChunkInput{{Stream: "build", Seq: 1, Body: "alpha"}, {Stream: "build", Seq: 2, Body: "beta"}, {Stream: "phase", Seq: 1, Body: "gamma"}}
	if res, err := f.append(ctx, 5, batch...); err != nil || res.Accepted != 3 {
		t.Fatalf("first = %+v, %v", res, err)
	}
	// Exact replay (an agent retry after a lost response): no error, no rows.
	res, err := f.append(ctx, 5, batch...)
	if err != nil || res.Accepted != 0 || res.Duplicates != 3 || res.Conflicts != 0 {
		t.Fatalf("replay = %+v, %v", res, err)
	}
	// Same key, different body: first write wins, reported as a conflict.
	res, err = f.append(ctx, 5, LogChunkInput{Stream: "build", Seq: 1, Body: "ALTERED"}, LogChunkInput{Stream: "build", Seq: 3, Body: "delta"})
	if err != nil || res.Conflicts != 1 || res.Accepted != 1 {
		t.Fatalf("conflict = %+v, %v", res, err)
	}
	var body string
	if err = f.pool.QueryRow(ctx, `SELECT body FROM deployment_log_chunks WHERE attempt_id=$1 AND stream='build' AND writer_epoch=5 AND seq=1`, f.attemptID).Scan(&body); err != nil || body != "alpha" {
		t.Fatalf("stored body = %q, %v", body, err)
	}
	// A restarted agent (new epoch) reusing seq 1 is a distinct key.
	if res, err = f.append(ctx, 6, LogChunkInput{Stream: "build", Seq: 1, Body: "alpha"}); err != nil || res.Accepted != 1 {
		t.Fatalf("new epoch = %+v, %v", res, err)
	}
	rows, bytes, _ := f.chunkCount(t, f.attemptID)
	var tb int64
	var tc int
	if err = f.pool.QueryRow(ctx, `SELECT bytes, chunks FROM deployment_log_attempt_totals WHERE attempt_id=$1`, f.attemptID).Scan(&tb, &tc); err != nil || tb != bytes || tc != rows || rows != 5 {
		t.Fatalf("totals bytes=%d chunks=%d vs rows=%d bytes=%d err=%v", tb, tc, rows, bytes, err)
	}
	// Invalid input is rejected whole (nothing written).
	for name, in := range map[string][]LogChunkInput{
		"bad stream": {{Stream: "stderr", Seq: 1, Body: "x"}},
		"empty body": {{Stream: "build", Seq: 1, Body: ""}},
		"oversize":   {{Stream: "build", Seq: 1, Body: strings.Repeat("a", MaxLogChunkBytes+1)}},
		"seq zero":   {{Stream: "build", Seq: 0, Body: "x"}},
		"bad utf8":   {{Stream: "build", Seq: 9, Body: "a\xffb"}},
		"nul":        {{Stream: "build", Seq: 9, Body: "a\x00b"}},
		"none":       nil,
		"too many":   lines("build", 100, MaxLogChunksPerAppend+1, "x"),
	} {
		if _, err = f.append(ctx, 5, in...); !errors.Is(err, ErrAgentLeaseInvalidInput) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if _, err = f.append(ctx, 0, LogChunkInput{Stream: "build", Seq: 9, Body: "x"}); !errors.Is(err, ErrAgentLeaseInvalidInput) {
		t.Fatalf("epoch 0 err = %v", err)
	}
	if r, _, _ := f.chunkCount(t, f.attemptID); r != 5 {
		t.Fatalf("invalid input wrote rows: %d", r)
	}
}

// concurrentAppend runs workers goroutines, each shipping perWorker chunks of
// body with disjoint seq ranges, 32 per call, all released together.
func (f logFixture) concurrentAppend(t *testing.T, workers, perWorker int, body string) {
	t.Helper()
	ctx := context.Background()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for sent := 0; sent < perWorker; sent += MaxLogChunksPerAppend {
				n := min(MaxLogChunksPerAppend, perWorker-sent)
				if _, err := f.append(ctx, 1, lines("build", w*perWorker+sent+1, n, body)...); err != nil {
					t.Errorf("worker %d: %v", w, err)
					return
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()
}

func (f logFixture) assertCapped(t *testing.T) {
	t.Helper()
	rows, bytes, markers := f.chunkCount(t, f.attemptID)
	if markers != 1 {
		t.Fatalf("truncation markers = %d, want exactly 1", markers)
	}
	if rows > MaxLogAttemptChunks || bytes > MaxLogAttemptBytes {
		t.Fatalf("caps exceeded: rows=%d bytes=%d", rows, bytes)
	}
	var tb int64
	var tc int
	var trunc bool
	if err := f.pool.QueryRow(context.Background(), `SELECT bytes, chunks, truncated FROM deployment_log_attempt_totals WHERE attempt_id=$1`, f.attemptID).Scan(&tb, &tc, &trunc); err != nil {
		t.Fatal(err)
	}
	if tb != bytes || tc != rows || !trunc {
		t.Fatalf("totals drifted from rows: totals=%d/%d trunc=%v rows=%d/%d", tb, tc, trunc, bytes, rows)
	}
}

func TestDeploymentLogsByteCapAndSingleMarkerUnderConcurrency(t *testing.T) {
	f := newLogFixture(t, 172)
	// 8 workers x 96 chunks x 4096 B = 3 MiB offered against a 256 KiB cap.
	f.concurrentAppend(t, 8, 96, strings.Repeat("b", MaxLogChunkBytes))
	f.assertCapped(t)
	_, bytes, _ := f.chunkCount(t, f.attemptID)
	if bytes < MaxLogAttemptBytes-MaxLogChunkBytes-int64(len(LogTruncationMarker)) {
		t.Fatalf("cap hit too early: %d bytes stored", bytes)
	}
	// Further appends are silently dropped: no error, no new rows.
	res, err := f.append(context.Background(), 1, lines("build", 90000, 3, "late")...)
	if err != nil || res.Dropped != 3 || res.Accepted != 0 {
		t.Fatalf("post-cap append = %+v, %v", res, err)
	}
	f.assertCapped(t)
}

func TestDeploymentLogsChunkCapAndSingleMarkerUnderConcurrency(t *testing.T) {
	f := newLogFixture(t, 173)
	// 8 x 640 one-byte chunks = 5120 offered against 4000 rows.
	f.concurrentAppend(t, 8, 640, "x")
	f.assertCapped(t)
	rows, _, _ := f.chunkCount(t, f.attemptID)
	if rows != MaxLogAttemptChunks {
		t.Fatalf("rows = %d, want exactly %d (cap includes the marker)", rows, MaxLogAttemptChunks)
	}
}

func TestDeploymentLogsAppendOnlyEnforcedForLiveRows(t *testing.T) {
	f := newLogFixture(t, 174)
	ctx := context.Background()
	if _, err := f.append(ctx, 1, LogChunkInput{Stream: "build", Seq: 1, Body: "keep"}); err != nil {
		t.Fatal(err)
	}
	var pgErr *pgconn.PgError
	if _, err := f.pool.Exec(ctx, `UPDATE deployment_log_chunks SET body='x' WHERE attempt_id=$1`, f.attemptID); !errors.As(err, &pgErr) || pgErr.Message != "deployment log chunks are append-only" {
		t.Fatalf("update err = %v", err)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM deployment_log_chunks WHERE attempt_id=$1`, f.attemptID); !errors.As(err, &pgErr) || !strings.Contains(pgErr.Message, "retention purge") {
		t.Fatalf("delete err = %v", err)
	}
	if rows, _, _ := f.chunkCount(t, f.attemptID); rows != 1 {
		t.Fatalf("rows = %d", rows)
	}
}

func TestDeploymentLogsAppendAuthorization(t *testing.T) {
	f := newLogFixture(t, 175)
	ctx := context.Background()
	f.insertServer(t, "server-other")
	otherDigest := digest(99)
	if _, err := f.pool.Exec(ctx, `INSERT INTO server_credentials(server_id,version,digest,status) VALUES ('server-other',1,$1,'active')`, otherDigest[:]); err != nil {
		t.Fatal(err)
	}
	one := LogChunkInput{Stream: "build", Seq: 1, Body: "x"}
	deny := func(name string, want error, res func() error) {
		t.Helper()
		err := res()
		if !errors.Is(err, ErrServerAuthorizationDenied) || (want != nil && !errors.Is(err, want)) {
			t.Fatalf("%s: err = %v, want denial wrapping %v", name, err, want)
		}
	}
	deny("wrong server", ErrLogServerMismatch, func() error {
		_, err := f.logs.AppendLogChunks(ctx, "server-other", otherDigest, f.attemptID, f.fence, 1, []LogChunkInput{one})
		return err
	})
	deny("wrong fence", ErrLogFenceMismatch, func() error {
		_, err := f.logs.AppendLogChunks(ctx, f.serverID, f.digest, f.attemptID, f.fence+1, 1, []LogChunkInput{one})
		return err
	})
	deny("unknown attempt", ErrLogAttemptNotFound, func() error {
		_, err := f.logs.AppendLogChunks(ctx, f.serverID, f.digest, f.attemptID+1000, f.fence, 1, []LogChunkInput{one})
		return err
	})
	deny("bad credential", nil, func() error {
		_, err := f.logs.AppendLogChunks(ctx, f.serverID, otherDigest, f.attemptID, f.fence, 1, []LogChunkInput{one})
		return err
	})
	if rows, _, _ := f.chunkCount(t, f.attemptID); rows != 0 {
		t.Fatalf("denied appends wrote %d rows", rows)
	}

	// Leased attempt accepted; closed 29 minutes ago still accepted; closed
	// 31 minutes ago refused; slot ownership is irrelevant after close.
	if res, err := f.append(ctx, 1, one); err != nil || res.Accepted != 1 {
		t.Fatalf("leased = %+v, %v", res, err)
	}
	f.closeAttempt(t, f.attemptID, "29 minutes")
	if res, err := f.append(ctx, 1, LogChunkInput{Stream: "drain", Seq: 1, Body: "drained"}); err != nil || res.Accepted != 1 {
		t.Fatalf("closed 29m = %+v, %v", res, err)
	}
	f.closeAttempt(t, f.attemptID, "31 minutes")
	deny("closed 31m", ErrLogWindowClosed, func() error {
		_, err := f.append(ctx, 1, LogChunkInput{Stream: "drain", Seq: 2, Body: "late"})
		return err
	})
	if rows, _, _ := f.chunkCount(t, f.attemptID); rows != 2 {
		t.Fatalf("rows = %d, want 2", rows)
	}
}

// TestDeploymentLogsAppendNeverBlocksOnLeaseLocks proves both directions:
// an append returns promptly while a Complete-like transaction holds FOR
// UPDATE on the attempt, job, deployment and slot rows (control probes show
// the locks are real), and an open append transaction does not block those
// same FOR UPDATE statements.
func TestDeploymentLogsAppendNeverBlocksOnLeaseLocks(t *testing.T) {
	f := newLogFixture(t, 176)
	ctx := context.Background()
	if _, err := f.append(ctx, 1, LogChunkInput{Stream: "build", Seq: 1, Body: "warm"}); err != nil {
		t.Fatal(err)
	}

	writer, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx) //nolint:errcheck
	for _, q := range []string{
		`SELECT 1 FROM agent_job_attempts WHERE id=$1 AND $2::bigint IS NOT NULL AND $3::text IS NOT NULL FOR UPDATE`,
		`SELECT 1 FROM agent_jobs WHERE id=$2 AND $1::bigint IS NOT NULL AND $3::text IS NOT NULL FOR UPDATE`,
		`SELECT 1 FROM deployments WHERE id=(SELECT deployment_id FROM agent_jobs WHERE id=$2) AND $1::bigint IS NOT NULL AND $3::text IS NOT NULL FOR UPDATE`,
		`SELECT 1 FROM environment_slots WHERE environment_id=$3 AND $1::bigint IS NOT NULL AND $2::bigint IS NOT NULL FOR UPDATE`,
	} {
		if _, err = writer.Exec(ctx, q, f.attemptID, f.jobID, f.envID); err != nil {
			t.Fatal(err)
		}
	}
	// Controls: the locks are real. An FK-style FOR KEY SHARE on the attempt
	// and a competing FOR UPDATE on the job both time out.
	for _, q := range []string{
		`SELECT 1 FROM agent_job_attempts WHERE id=$1 AND $2::bigint IS NOT NULL FOR KEY SHARE`,
		`SELECT 1 FROM agent_jobs WHERE id=$2 AND $1::bigint IS NOT NULL AND $3::text IS NOT NULL FOR UPDATE`,
	} {
		probe, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		_, perr := f.pool.Exec(probe, q, f.attemptID, f.jobID)
		cancel()
		if perr == nil {
			t.Fatalf("control %q was not blocked; the writer holds no lock", q)
		}
	}

	short, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	start := time.Now()
	res, err := f.append(short, 1, LogChunkInput{Stream: "build", Seq: 2, Body: "while locked"})
	if err != nil || res.Accepted != 1 {
		t.Fatalf("append under held lease locks = %+v, %v", res, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("append took %v under held lease locks", time.Since(start))
	}
	if err = writer.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// Reverse direction: an append transaction left open (same statements the
	// store runs, uncommitted) must not hold up the lease-side lockers.
	open, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer open.Rollback(ctx) //nolint:errcheck
	if _, err = open.Exec(ctx, `INSERT INTO deployment_log_attempt_totals(attempt_id) VALUES (999) ON CONFLICT (attempt_id) DO UPDATE SET updated_at=clock_timestamp()`); err != nil {
		t.Fatal(err)
	}
	if _, err = open.Exec(ctx, `INSERT INTO deployment_log_chunks(attempt_id, stream, writer_epoch, seq, body) VALUES ($1,'build',1,50,'open tx')`, f.attemptID); err != nil {
		t.Fatal(err)
	}
	probe, cancelProbe := context.WithTimeout(ctx, 2*time.Second)
	defer cancelProbe()
	for _, q := range []string{
		`SELECT 1 FROM agent_job_attempts WHERE id=$1 AND $2::bigint IS NOT NULL AND $3::text IS NOT NULL FOR UPDATE`,
		`SELECT 1 FROM agent_jobs WHERE id=$2 AND $1::bigint IS NOT NULL AND $3::text IS NOT NULL FOR UPDATE`,
		`SELECT 1 FROM deployments WHERE id=(SELECT deployment_id FROM agent_jobs WHERE id=$2) AND $1::bigint IS NOT NULL AND $3::text IS NOT NULL FOR UPDATE`,
		`SELECT 1 FROM environment_slots WHERE environment_id=$3 AND $1::bigint IS NOT NULL AND $2::bigint IS NOT NULL FOR UPDATE`,
	} {
		if _, err = f.pool.Exec(probe, q, f.attemptID, f.jobID, f.envID); err != nil {
			t.Fatalf("lease-side lock %q blocked by an open log append: %v", q, err)
		}
	}
	if _, err = f.pool.Exec(probe, `UPDATE agent_job_attempts SET renewal_sequence=renewal_sequence WHERE id=$1`, f.attemptID); err != nil {
		t.Fatalf("attempt update blocked by an open log append: %v", err)
	}
}

// seedAttempt inserts an attempt directly (triggers off) and returns its id.
func (f logFixture) seedAttempt(t *testing.T, jobID int64, number int, env string, closedAgo string, chunks int) int64 {
	t.Helper()
	var id int64
	asReplica(t, f.pool, func(tx pgx.Tx) {
		ctx := context.Background()
		if err := tx.QueryRow(ctx, `WITH t AS (SELECT clock_timestamp()-interval '1 hour' AS d)
			INSERT INTO agent_job_attempts(job_id, attempt_number, server_id, environment_id, fence, state, failure_class, result_operation_id, result_digest, lease_expires_at, authority_expires_at, closed_at)
			SELECT $1,$2,$3,$4,1,'failed','transient','op',decode(repeat('00',32),'hex'),t.d,t.d,clock_timestamp()-$5::interval FROM t RETURNING id`,
			jobID, number, f.serverID, env, closedAgo).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if chunks > 0 {
			if _, err := tx.Exec(ctx, `INSERT INTO deployment_log_chunks(attempt_id, stream, writer_epoch, seq, body) SELECT $1,'build',1,g,'row' FROM generate_series(1,$2) g`, id, chunks); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO deployment_log_attempt_totals(attempt_id, bytes, chunks) VALUES ($1,$2*3,$2)`, id, chunks); err != nil {
				t.Fatal(err)
			}
		}
	})
	return id
}

// seedRetentionSet builds environment env-p (25 attempts, all closed 20 days
// ago) and env-q (23 attempts, the oldest closed 1 day ago, the rest 20 days
// ago), 3 chunks each. Expected purge: env-p's 5 oldest; env-q's 2 oldest
// except the young oldest -> exactly attempts ranked 21+ by id AND aged.
func (f logFixture) seedRetentionSet(t *testing.T, chunksPer int) (purgeable, protected []int64) {
	t.Helper()
	ctx := context.Background()
	for _, env := range []string{"env-p", "env-q"} {
		if _, err := f.pool.Exec(ctx, `INSERT INTO environment_slots(environment_id) VALUES ($1) ON CONFLICT DO NOTHING`, env); err != nil {
			t.Fatal(err)
		}
	}
	var jobs []int64
	for i := 0; i < 10; i++ {
		jobs = append(jobs, f.insertJob(t, f.serverID, "environment", fmt.Sprintf("purge-%d", i), 0))
	}
	next := 0
	seed := func(env, ago string) int64 {
		job := jobs[next/5]
		number := next%5 + 1
		next++
		return f.seedAttempt(t, job, number, env, ago, chunksPer)
	}
	for i := 0; i < 25; i++ {
		id := seed("env-p", "20 days")
		if i < 5 {
			purgeable = append(purgeable, id)
		} else {
			protected = append(protected, id)
		}
	}
	for i := 0; i < 23; i++ {
		ago := "20 days"
		if i == 0 {
			ago = "1 day"
		}
		id := seed("env-q", ago)
		if i == 1 || i == 2 {
			purgeable = append(purgeable, id)
		} else {
			protected = append(protected, id)
		}
	}
	return
}

func (f logFixture) liveAttempts(t *testing.T) (chunks, totals map[int64]int) {
	t.Helper()
	chunks, totals = map[int64]int{}, map[int64]int{}
	for q, m := range map[string]map[int64]int{
		`SELECT attempt_id, count(*) FROM deployment_log_chunks GROUP BY 1`: chunks,
		`SELECT attempt_id, chunks FROM deployment_log_attempt_totals`:      totals,
	} {
		rows, err := f.pool.Query(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id int64
			var n int
			if err = rows.Scan(&id, &n); err != nil {
				t.Fatal(err)
			}
			m[id] = n
		}
		rows.Close()
	}
	return
}

func TestDeploymentLogsPurgeRetentionAndBatchBound(t *testing.T) {
	f := newLogFixture(t, 177)
	ctx := context.Background()
	// The fixture's own live attempt (leased, with chunks) must survive too.
	if _, err := f.append(ctx, 1, lines("build", 1, 3, "live")...); err != nil {
		t.Fatal(err)
	}
	purgeable, protected := f.seedRetentionSet(t, 3)

	if _, err := f.logs.PurgeLogChunks(ctx, time.Now(), LogRetention{}, 10); !errors.Is(err, ErrOperationsInvalidInput) {
		t.Fatalf("zero retention err = %v", err)
	}
	if _, err := f.logs.PurgeLogChunks(ctx, time.Now(), DefaultLogRetention, 0); !errors.Is(err, ErrOperationsInvalidInput) {
		t.Fatalf("zero batch err = %v", err)
	}

	// Batch bound: 4 rows at a time, never more.
	first, err := f.logs.PurgeLogChunks(ctx, time.Now(), DefaultLogRetention, 4)
	if err != nil || first != 4 {
		t.Fatalf("first batch = %d, %v", first, err)
	}
	chunks, totals := f.liveAttempts(t)
	// Oldest attempt fully gone (3 rows + its totals); the next is partial
	// (2 rows left) and keeps its accurate-enough totals row.
	if _, ok := chunks[purgeable[0]]; ok {
		t.Fatalf("oldest purgeable attempt still has chunks")
	}
	if _, ok := totals[purgeable[0]]; ok {
		t.Fatalf("emptied attempt kept its totals row")
	}
	if chunks[purgeable[1]] != 2 {
		t.Fatalf("partial attempt chunks = %d, want 2", chunks[purgeable[1]])
	}
	if _, ok := totals[purgeable[1]]; !ok {
		t.Fatalf("partially purged attempt lost its totals row")
	}
	// The purge GUC must not leak: a plain delete on the pool still fails.
	if _, err = f.pool.Exec(ctx, `DELETE FROM deployment_log_chunks WHERE attempt_id=$1`, protected[0]); err == nil {
		t.Fatal("DELETE succeeded outside the purge: GUC leaked")
	}

	var total int64 = first
	for i := 0; i < 20; i++ {
		n, err := f.logs.PurgeLogChunks(ctx, time.Now(), DefaultLogRetention, 4)
		if err != nil {
			t.Fatal(err)
		}
		if n > 4 {
			t.Fatalf("batch of %d exceeds limit 4", n)
		}
		total += n
		if n == 0 {
			break
		}
	}
	if want := int64(len(purgeable) * 3); total != want {
		t.Fatalf("purged %d rows, want %d", total, want)
	}
	chunks, totals = f.liveAttempts(t)
	for _, id := range purgeable {
		if _, ok := chunks[id]; ok {
			t.Fatalf("purgeable attempt %d still has chunks", id)
		}
		if _, ok := totals[id]; ok {
			t.Fatalf("purgeable attempt %d kept totals", id)
		}
	}
	for _, id := range protected {
		if chunks[id] != 3 || totals[id] != 3 {
			t.Fatalf("protected attempt %d chunks=%d totals=%d", id, chunks[id], totals[id])
		}
	}
	if chunks[f.attemptID] != 3 {
		t.Fatalf("live attempt chunks = %d", chunks[f.attemptID])
	}
	// Idempotent: nothing left to purge.
	if n, err := f.logs.PurgeLogChunks(ctx, time.Now(), DefaultLogRetention, 100); err != nil || n != 0 {
		t.Fatalf("second purge = %d, %v", n, err)
	}
	// A later "now" past 14 days also ages the young attempt, but it is still
	// outside... it is the oldest of env-q (rank 23) so it becomes purgeable.
	n, err := f.logs.PurgeLogChunks(ctx, time.Now().Add(15*24*time.Hour), DefaultLogRetention, 100)
	if err != nil || n != 3 {
		t.Fatalf("aged purge = %d, %v (want only the formerly young rank-23 attempt)", n, err)
	}
}

// TestDeploymentLogsQueryPlans seeds a larger set and records the plans. It
// asserts the list read uses an index on the chunk table (no seq scan) and the
// purge never seq-scans the chunk table.
func TestDeploymentLogsQueryPlans(t *testing.T) {
	f := newLogFixture(t, 178)
	ctx := context.Background()
	f.seedRetentionSet(t, 400) // 48 attempts x 400 = 19200 chunks
	if _, err := f.append(ctx, 1, lines("build", 1, 30, "live")...); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `ANALYZE deployment_log_chunks, deployment_log_attempt_totals, agent_job_attempts, agent_jobs`); err != nil {
		t.Fatal(err)
	}
	explain := func(name, q string, args ...any) string {
		t.Helper()
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err = tx.Exec(ctx, `SELECT set_config('autodeploy.log_purge','on',true)`); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(q, "/*forced*/") {
			if _, err = tx.Exec(ctx, `SET LOCAL enable_seqscan=off`); err != nil {
				t.Fatal(err)
			}
		}
		rows, err := tx.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS OFF, COSTS OFF, TIMING OFF) `+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var sb strings.Builder
		for rows.Next() {
			var l string
			if err = rows.Scan(&l); err != nil {
				t.Fatal(err)
			}
			sb.WriteString(l + "\n")
		}
		t.Logf("PLAN %s:\n%s", name, sb.String())
		return sb.String()
	}
	list := explain("list", `SELECT c.id, c.attempt_id, a.attempt_number, c.stream, c.seq, c.body, c.created_at
FROM agent_job_attempts a JOIN deployment_log_chunks c ON c.attempt_id=a.id
WHERE a.job_id=$1 AND c.id>$2 ORDER BY c.id LIMIT 201`, f.jobID, int64(0))
	if strings.Contains(list, "Seq Scan on deployment_log_chunks") {
		t.Fatalf("list seq-scans the chunk table")
	}
	purge := explain("purge", logPurgeSQL, time.Now().Add(-14*24*time.Hour), 20, 500)
	if strings.Contains(purge, "Seq Scan on deployment_log_chunks") {
		t.Fatalf("purge seq-scans the chunk table")
	}
	// The newest-N lookup per environment must be able to use the attempts
	// index (the 48-row table is too small for the planner to prefer it).
	forced := explain("purge, seqscan off", "/*forced*/ "+logPurgeSQL, time.Now().Add(-14*24*time.Hour), 20, 500)
	if !strings.Contains(forced, "agent_job_attempts_environment_id_idx") {
		t.Fatalf("newest-per-environment lookup does not use agent_job_attempts_environment_id_idx when seq scans are disabled")
	}
}

// TestLogPurgeSchedulerThroughSweeperWiring drives the real sweeper tick loop
// (real pool, advisory locks, repository) and proves old attempts are purged
// while the newest 20 per environment, young attempts and the live attempt
// survive; a second scheduler proves the single-runner try-lock.
func TestLogPurgeSchedulerThroughSweeperWiring(t *testing.T) {
	f := newLogFixture(t, 181)
	ctx := context.Background()
	if _, err := f.append(ctx, 1, lines("build", 1, 3, "live")...); err != nil {
		t.Fatal(err)
	}
	purgeable, protected := f.seedRetentionSet(t, 3)

	// Single runner: while another session holds the purge lock, a run skips.
	holder, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got bool
	if err = holder.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, deploymentLogPurgeAdvisoryLockKey).Scan(&got); err != nil || !got {
		t.Fatalf("holder lock = %v, %v", got, err)
	}
	sched, err := NewLogPurgeScheduler(f.pool, f.logs, LogPurgeOptions{Retention: DefaultLogRetention, Every: time.Millisecond, BatchSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	sched.Trigger(ctx)
	sched.Wait(10 * time.Second)
	if chunks, _ := f.liveAttempts(t); chunks[purgeable[0]] == 0 {
		t.Fatal("purge ran while another session held the lock")
	}
	if _, err = holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, deploymentLogPurgeAdvisoryLockKey); err != nil {
		t.Fatal(err)
	}
	holder.Release()

	// Real sweeper wiring: ticks run lease duties, then the purge drains in
	// 4-row batches until nothing purgeable remains.
	sweeper := NewAgentLeaseSweeper(f.pool, NewAgentLeaseRepository(f.pool, NewDeploymentRepository(f.pool))).WithTick(10 * time.Millisecond).WithLogPurge(sched)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); sweeper.Run(runCtx) }()
	deadline := time.After(20 * time.Second)
	for {
		chunks, _ := f.liveAttempts(t)
		remaining := 0
		for _, id := range purgeable {
			remaining += chunks[id]
		}
		if remaining == 0 {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatalf("purgeable chunks remaining = %d", remaining)
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done
	chunks, totals := f.liveAttempts(t)
	for _, id := range purgeable {
		if _, ok := totals[id]; ok {
			t.Fatalf("purged attempt %d kept totals", id)
		}
	}
	for _, id := range protected {
		if chunks[id] != 3 || totals[id] != 3 {
			t.Fatalf("protected attempt %d chunks=%d totals=%d", id, chunks[id], totals[id])
		}
	}
	if chunks[f.attemptID] != 3 {
		t.Fatalf("live attempt chunks = %d", chunks[f.attemptID])
	}
	var held bool
	if err = f.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype='advisory' AND granted AND objid=$1)`, int32(deploymentLogPurgeAdvisoryLockKey&0xffffffff)).Scan(&held); err != nil || held {
		t.Fatalf("purge advisory lock leaked: held=%v err=%v", held, err)
	}
}
