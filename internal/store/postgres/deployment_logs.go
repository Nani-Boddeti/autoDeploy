package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"autodeploy/internal/auth"
	"autodeploy/internal/github"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DeploymentLogRepository stores and serves the deployment log channel
// (step 15g): agent-appended chunks, owner-scoped reads and the retention
// purge.
//
// Locking rules (the point of this type): appends take NO lock on slots,
// jobs, deployments or attempts. The attempt is read with a plain SELECT and
// the chunk/totals tables carry no foreign key to it (an FK insert takes FOR
// KEY SHARE on the attempt row, which conflicts with the FOR UPDATE that
// Complete/Activate/Renew take). The only locks are the shared server and
// credential row locks of lockActiveServerCredential (as every agent call
// takes) and the row lock on the attempt's own totals row.
type DeploymentLogRepository struct {
	pool *pgxpool.Pool
}

func NewDeploymentLogRepository(pool *pgxpool.Pool) *DeploymentLogRepository {
	return &DeploymentLogRepository{pool: pool}
}

const (
	// MaxLogChunkBytes bounds one chunk body.
	MaxLogChunkBytes = 4096
	// MaxLogChunksPerAppend bounds one AppendLogChunks call.
	MaxLogChunksPerAppend = 32
	// MaxLogAttemptBytes and MaxLogAttemptChunks are the per-attempt caps. They
	// include the truncation marker row, so stored body bytes never exceed
	// MaxLogAttemptBytes and stored rows never exceed MaxLogAttemptChunks.
	MaxLogAttemptBytes  = 256 * 1024
	MaxLogAttemptChunks = 4000
	// LogPostCloseWindow is how long after an attempt closed its own server
	// may still append (activation report + drain run after the lease ends).
	LogPostCloseWindow = 30 * time.Minute
	// MaxLogPageLimit bounds one ListDeploymentLogs page.
	MaxLogPageLimit = 200

	// LogTruncationMarker is the single body written when an attempt first hits
	// a cap. It is stored as stream 'system', writer_epoch 0, seq 1.
	LogTruncationMarker = "[log truncated: per-attempt limit reached]"
)

// Typed denial sentinels. Each is joined with ErrServerAuthorizationDenied so
// callers (the 15g-2 endpoint) can keep the generic 403/404 mapping and not
// leak which check failed, while tests and logs can still tell them apart.
var (
	ErrLogAttemptNotFound = errors.New("log attempt not found")
	ErrLogServerMismatch  = errors.New("log attempt belongs to another server")
	ErrLogFenceMismatch   = errors.New("log attempt fence mismatch")
	ErrLogWindowClosed    = errors.New("log attempt closed beyond the post-close window")
)

func logDenied(reason error) error {
	return fmt.Errorf("%w: %w", ErrServerAuthorizationDenied, reason)
}

var logStreams = map[string]struct{}{"build": {}, "phase": {}, "health": {}, "activation": {}, "drain": {}, "system": {}}

// LogChunkInput is one agent-supplied chunk. Seq is per (stream, epoch).
type LogChunkInput struct {
	Stream string
	Seq    int64
	Body   string
}

// AppendLogResult reports what one AppendLogChunks call did. Accepted +
// Duplicates + Conflicts + Dropped == len(chunks). Marker is true when this
// call wrote the truncation marker.
type AppendLogResult struct {
	Accepted   int
	Duplicates int
	// Conflicts counts replays of an existing key whose body differs. The
	// stored (first) body wins and nothing is overwritten (rows are
	// immutable); the caller may log the count. It is not an error: failing
	// would only make the agent stop shipping, and the stored content is
	// already authoritative.
	Conflicts int
	Dropped   int
	Marker    bool
}

func validLogChunk(c LogChunkInput) bool {
	if _, ok := logStreams[c.Stream]; !ok || c.Seq <= 0 {
		return false
	}
	if len(c.Body) < 1 || len(c.Body) > MaxLogChunkBytes || !utf8.ValidString(c.Body) {
		return false
	}
	for i := 0; i < len(c.Body); i++ {
		if c.Body[i] == 0 {
			return false
		}
	}
	return true
}

// AppendLogChunks stores chunks for an attempt on behalf of its own server.
//
// Authorization (no lock on slot/job/deployment/attempt): active credential,
// attempt exists, attempt.server_id == serverID, attempt.fence == fence, and
// the attempt is leased or closed less than LogPostCloseWindow ago. The slot
// is NOT consulted: after close it may already belong to a newer attempt and
// the attempt's own fence is the authority.
//
// Cap race-safety: the first statement upserts the attempt's totals row
// (INSERT ... ON CONFLICT DO UPDATE ... RETURNING), which takes that row's
// lock and returns its latest committed counters. Concurrent appends for the
// same attempt therefore serialize on one row in a table only this path
// writes, so the check-then-add of bytes/chunks cannot be raced past the cap,
// while appends for different attempts and every lease transaction (which
// never touch this table or the attempt via FK) are unaffected. A per-attempt
// advisory lock would give the same serialization but share a global key
// space with the sweeper's locks; the row lock is scoped and visible.
//
// Replay: a chunk whose (stream, epoch, seq) already exists is a duplicate
// and is not counted again. Once the attempt is truncated (or a chunk does
// not fit), the chunk and all later ones are dropped silently; the first cap
// hit writes exactly one marker row (system, epoch 0, seq 1) and sets
// truncated.
func (r *DeploymentLogRepository) AppendLogChunks(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID, fence, writerEpoch int64, chunks []LogChunkInput) (AppendLogResult, error) {
	if r == nil || r.pool == nil || !github.ValidOpaqueID(serverID) || attemptID <= 0 || fence <= 0 || writerEpoch <= 0 ||
		len(chunks) == 0 || len(chunks) > MaxLogChunksPerAppend {
		return AppendLogResult{}, ErrAgentLeaseInvalidInput
	}
	for _, c := range chunks {
		if !validLogChunk(c) {
			return AppendLogResult{}, ErrAgentLeaseInvalidInput
		}
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return AppendLogResult{}, fmt.Errorf("begin log append: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = lockActiveServerCredential(ctx, tx, serverID, presentedDigest); err != nil {
		return AppendLogResult{}, err
	}

	var attemptServerID, state string
	var attemptFence int64
	var inWindow bool
	err = tx.QueryRow(ctx, `SELECT server_id, fence, state, state='leased' OR closed_at > clock_timestamp() - make_interval(secs => $2) FROM agent_job_attempts WHERE id=$1`,
		attemptID, LogPostCloseWindow.Seconds()).Scan(&attemptServerID, &attemptFence, &state, &inWindow)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return AppendLogResult{}, logDenied(ErrLogAttemptNotFound)
	case err != nil:
		return AppendLogResult{}, fmt.Errorf("locate log attempt: %w", err)
	case attemptServerID != serverID:
		return AppendLogResult{}, logDenied(ErrLogServerMismatch)
	case attemptFence != fence:
		return AppendLogResult{}, logDenied(ErrLogFenceMismatch)
	case !inWindow:
		return AppendLogResult{}, logDenied(ErrLogWindowClosed)
	}

	var bytes int64
	var count int
	var truncated bool
	if err = tx.QueryRow(ctx, `INSERT INTO deployment_log_attempt_totals(attempt_id) VALUES ($1)
ON CONFLICT (attempt_id) DO UPDATE SET updated_at=clock_timestamp()
RETURNING bytes, chunks, truncated`, attemptID).Scan(&bytes, &count, &truncated); err != nil {
		return AppendLogResult{}, fmt.Errorf("lock log totals: %w", err)
	}

	// Room is measured against the caps minus the marker, so the marker row
	// always fits and the hard caps hold including it.
	const bodyRoom = MaxLogAttemptBytes - len(LogTruncationMarker)
	const rowRoom = MaxLogAttemptChunks - 1

	// existing reports a stored row for the chunk's key and whether its body
	// matches, so a replay is never mistaken for new data (even at the cap).
	var res AppendLogResult
	existing := func(c LogChunkInput) (found, same bool, err error) {
		var body string
		err = tx.QueryRow(ctx, `SELECT body FROM deployment_log_chunks WHERE attempt_id=$1 AND stream=$2 AND writer_epoch=$3 AND seq=$4`,
			attemptID, c.Stream, writerEpoch, c.Seq).Scan(&body)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, false, nil
		}
		if err != nil {
			return false, false, fmt.Errorf("read replayed log chunk: %w", err)
		}
		return true, body == c.Body, nil
	}
	recordReplay := func(same bool) {
		if same {
			res.Duplicates++
		} else {
			res.Conflicts++
		}
	}

	for i, c := range chunks {
		if truncated {
			res.Dropped += len(chunks) - i
			break
		}
		if count+1 > rowRoom || bytes+int64(len(c.Body)) > int64(bodyRoom) {
			found, same, err := existing(c)
			if err != nil {
				return AppendLogResult{}, err
			}
			if found {
				recordReplay(same)
				continue
			}
			var markerID int64
			err = tx.QueryRow(ctx, `INSERT INTO deployment_log_chunks(attempt_id, stream, writer_epoch, seq, body) VALUES ($1,'system',0,1,$2)
ON CONFLICT (attempt_id, stream, writer_epoch, seq) DO NOTHING RETURNING id`, attemptID, LogTruncationMarker).Scan(&markerID)
			switch {
			case err == nil:
				bytes += int64(len(LogTruncationMarker))
				count++
				res.Marker = true
			case !errors.Is(err, pgx.ErrNoRows):
				return AppendLogResult{}, fmt.Errorf("insert log truncation marker: %w", err)
			}
			truncated = true
			res.Dropped += len(chunks) - i
			break
		}
		var id int64
		err = tx.QueryRow(ctx, `INSERT INTO deployment_log_chunks(attempt_id, stream, writer_epoch, seq, body) VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (attempt_id, stream, writer_epoch, seq) DO NOTHING RETURNING id`, attemptID, c.Stream, writerEpoch, c.Seq, c.Body).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			_, same, err := existing(c)
			if err != nil {
				return AppendLogResult{}, err
			}
			recordReplay(same)
			continue
		}
		if err != nil {
			return AppendLogResult{}, fmt.Errorf("insert log chunk: %w", err)
		}
		bytes += int64(len(c.Body))
		count++
		res.Accepted++
	}

	if _, err = tx.Exec(ctx, `UPDATE deployment_log_attempt_totals SET bytes=$2, chunks=$3, truncated=$4, updated_at=clock_timestamp() WHERE attempt_id=$1`,
		attemptID, bytes, count, truncated); err != nil {
		return AppendLogResult{}, fmt.Errorf("update log totals: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return AppendLogResult{}, fmt.Errorf("commit log append: %w", err)
	}
	return res, nil
}

// DeploymentLogLine is one stored chunk with its attempt number.
type DeploymentLogLine struct {
	ID            int64
	AttemptID     int64
	AttemptNumber int
	Stream        string
	Seq           int64
	Body          string
	CreatedAt     time.Time
}

// DeploymentLogPage is one oldest-first page. Next is non-nil iff more rows
// exist after this page; pass it as afterID for the following page.
type DeploymentLogPage struct {
	Items []DeploymentLogLine
	Next  *int64
}

// ListDeploymentLogs returns chunks of every attempt of the owned
// deployment's job with id > afterID, oldest first (id order), at most limit
// (0 = MaxLogPageLimit). Same read rules as OperationsReadRepository: owner
// scope from agent_jobs.owner_id, live session re-checked without locks in
// one read-only transaction, unknown/malformed/foreign deployment ->
// ErrOperationsNotFound.
func (r *DeploymentLogRepository) ListDeploymentLogs(ctx context.Context, principal auth.Principal, deploymentID string, afterID int64, limit int) (DeploymentLogPage, error) {
	if r == nil || r.pool == nil || principal.Validate() != nil {
		return DeploymentLogPage{}, ErrServerAuthorizationDenied
	}
	if limit == 0 {
		limit = MaxLogPageLimit
	}
	if limit < 0 || limit > MaxLogPageLimit || afterID < 0 {
		return DeploymentLogPage{}, ErrOperationsInvalidInput
	}
	if !github.ValidOpaqueID(deploymentID) {
		return DeploymentLogPage{}, ErrOperationsNotFound
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return DeploymentLogPage{}, fmt.Errorf("begin deployment log read: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err = requireLiveSession(ctx, tx, principal); err != nil {
		return DeploymentLogPage{}, err
	}
	var jobID int64
	if err = tx.QueryRow(ctx, `SELECT id FROM agent_jobs WHERE owner_id=$1 AND deployment_id=$2`, principal.OwnerID, deploymentID).Scan(&jobID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DeploymentLogPage{}, ErrOperationsNotFound
		}
		return DeploymentLogPage{}, fmt.Errorf("locate deployment job for logs: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT c.id, c.attempt_id, a.attempt_number, c.stream, c.seq, c.body, c.created_at
FROM agent_job_attempts a JOIN deployment_log_chunks c ON c.attempt_id=a.id
WHERE a.job_id=$1 AND c.id>$2
ORDER BY c.id LIMIT $3`, jobID, afterID, limit+1)
	if err != nil {
		return DeploymentLogPage{}, fmt.Errorf("list deployment logs: %w", err)
	}
	defer rows.Close()
	page := DeploymentLogPage{Items: []DeploymentLogLine{}}
	for rows.Next() {
		var l DeploymentLogLine
		if err = rows.Scan(&l.ID, &l.AttemptID, &l.AttemptNumber, &l.Stream, &l.Seq, &l.Body, &l.CreatedAt); err != nil {
			return DeploymentLogPage{}, fmt.Errorf("scan deployment log: %w", err)
		}
		page.Items = append(page.Items, l)
	}
	if err = rows.Err(); err != nil {
		return DeploymentLogPage{}, fmt.Errorf("list deployment logs: %w", err)
	}
	rows.Close()
	if err = tx.Commit(ctx); err != nil {
		return DeploymentLogPage{}, fmt.Errorf("commit deployment log read: %w", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		next := page.Items[limit-1].ID
		page.Next = &next
	}
	return page, nil
}

// LogRetention is the purge rule: chunks of an attempt are purgeable when it
// closed more than MaxAge ago AND it is not among the KeepNewestPerEnv newest
// attempts (by id) of its environment.
type LogRetention struct {
	MaxAge           time.Duration
	KeepNewestPerEnv int
}

// DefaultLogRetention is the owner-decided policy (15g L4).
var DefaultLogRetention = LogRetention{MaxAge: 14 * 24 * time.Hour, KeepNewestPerEnv: 20}

// logPurgeSQL ($1 cutoff, $2 keep-newest-per-env, $3 batch). eligible walks
// old attempts that still have chunks and rank below the newest-N of their
// environment (a.id below the N-th newest id; fewer than N attempts -> the
// subquery is NULL -> 0 -> nothing eligible). victims then takes chunk ids per
// eligible attempt through the (attempt_id, id) index, so the cost follows the
// batch, not the table. The final delete addresses rows by primary key.
const logPurgeSQL = `
WITH eligible AS (
  SELECT a.id FROM agent_job_attempts a
  WHERE a.closed_at < $1
    AND EXISTS (SELECT 1 FROM deployment_log_chunks c WHERE c.attempt_id=a.id)
    AND a.id < COALESCE((SELECT k.id FROM agent_job_attempts k WHERE k.environment_id=a.environment_id
                         ORDER BY k.id DESC OFFSET $2-1 LIMIT 1), 0)
  ORDER BY a.id LIMIT $3
), victims AS MATERIALIZED (
  SELECT v.id FROM eligible e CROSS JOIN LATERAL (
    SELECT c.id FROM deployment_log_chunks c WHERE c.attempt_id=e.id ORDER BY c.id LIMIT $3
  ) v ORDER BY v.id LIMIT $3
)
DELETE FROM deployment_log_chunks WHERE id = ANY(ARRAY(SELECT id FROM victims)) RETURNING attempt_id`

// PurgeLogChunks deletes at most batchLimit chunk rows of purgeable attempts
// (oldest chunk ids first) and then the totals rows of attempts left with no
// chunks, in one transaction that sets the transaction-local purge GUC the
// append-only trigger requires. It returns the number of chunk rows deleted;
// a result below batchLimit means nothing purgeable remains.
func (r *DeploymentLogRepository) PurgeLogChunks(ctx context.Context, now time.Time, retention LogRetention, batchLimit int) (int64, error) {
	if r == nil || r.pool == nil || now.IsZero() || retention.MaxAge <= 0 || retention.KeepNewestPerEnv < 1 || batchLimit < 1 {
		return 0, ErrOperationsInvalidInput
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin log purge: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT set_config('autodeploy.log_purge','on',true)`); err != nil {
		return 0, fmt.Errorf("enable log purge: %w", err)
	}
	cutoff := now.Add(-retention.MaxAge)
	rows, err := tx.Query(ctx, logPurgeSQL, cutoff, retention.KeepNewestPerEnv, batchLimit)
	if err != nil {
		return 0, fmt.Errorf("purge log chunks: %w", err)
	}
	touched := map[int64]struct{}{}
	var deleted int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan purged log chunk: %w", err)
		}
		touched[id] = struct{}{}
		deleted++
	}
	if err = rows.Err(); err != nil {
		return 0, fmt.Errorf("purge log chunks: %w", err)
	}
	rows.Close()
	if len(touched) > 0 {
		ids := make([]int64, 0, len(touched))
		for id := range touched {
			ids = append(ids, id)
		}
		// Only attempts that are now empty: an eligible attempt whose chunks
		// were cut short by the batch limit keeps its (accurate) totals.
		if _, err = tx.Exec(ctx, `DELETE FROM deployment_log_attempt_totals t WHERE t.attempt_id = ANY($1)
AND NOT EXISTS (SELECT 1 FROM deployment_log_chunks c WHERE c.attempt_id=t.attempt_id)`, ids); err != nil {
			return 0, fmt.Errorf("purge log totals: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit log purge: %w", err)
	}
	return deleted, nil
}
