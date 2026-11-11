package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"autodeploy/internal/github"
)

// Pending drain records (tasks/10e-plan.md, 10e-3). After the control plane
// acknowledges (HTTP 200) a successful activation report, the agent owes a
// stop of the previous release once drain_seconds have elapsed. The obligation
// is an agent-local, durable record outside any lease (owner decision D-a),
// written BEFORE the activation record that carried it is deleted. The store
// follows the ActivationStore rules: atomic writes, dir+parent fsync, shared
// per-directory lock, strict decode, fail closed.

const (
	drainsDirName     = "drains"
	drainFilePrefix   = "attempt-"
	drainFileSuffix   = ".json"
	maxDrainSummaryLn = maxFailureSummaryBytes
)

var (
	// ErrDrainImmutable: a save would change a field fixed at arming.
	ErrDrainImmutable = errors.New("pending drain immutable field changed")
	// ErrDrainRegressed: a save would lower Attempts or move stop_failed back
	// to pending without a higher Attempts.
	ErrDrainRegressed = errors.New("pending drain must not regress")
	// ErrInvalidDrain wraps every validation failure of a drain record.
	ErrInvalidDrain = errors.New("invalid pending drain")
)

// DrainPhase is the state of a drain obligation.
type DrainPhase string

const (
	// DrainPending: the stop is owed (or being retried).
	DrainPending DrainPhase = "pending"
	// DrainStopFailed: the last stop attempt failed; kept as evidence.
	DrainStopFailed DrainPhase = "stop_failed"
)

// PendingDrain is one durable obligation to stop a predecessor release.
type PendingDrain struct {
	AttemptID     int64
	EnvironmentID string
	// New release and runtime instance: must never be stopped.
	ReleaseID, BackendRuntimeInstanceID string
	// The stop target.
	PredecessorReleaseID, PredecessorRuntimeInstanceID string
	DrainSeconds                                       int
	ArmedAt                                            time.Time // UTC; preserved across re-saves
	ObservedRouteDigest                                [32]byte  // digest the switch left on disk
	Phase                                              DrainPhase
	FailureSummary                                     string // stop_failed only
	Attempts                                           int    // stop attempts so far (the drainer increments)
	// Retryable is meaningful only in stop_failed (false in pending): the
	// drainer may try again. Permanent failures (guard refusals, configuration
	// errors, exhausted attempts) are kept as non-retryable evidence.
	Retryable            bool
	CreatedAt, UpdatedAt time.Time
}

// DrainFromActivation derives the drain obligation of a successful result_ready
// activation record. ok is false when there is nothing to drain (failure
// result, or no predecessor).
func DrainFromActivation(p PendingActivation, armedAt time.Time) (PendingDrain, bool) {
	if p.Phase != PhaseResultReady || !p.Succeeded || !p.HasPredecessor() {
		return PendingDrain{}, false
	}
	return PendingDrain{
		AttemptID: p.AttemptID, EnvironmentID: p.EnvironmentID,
		ReleaseID: p.ReleaseID, BackendRuntimeInstanceID: p.BackendRuntimeInstanceID,
		PredecessorReleaseID:         p.PredecessorReleaseID,
		PredecessorRuntimeInstanceID: p.PredecessorRuntimeInstanceID,
		DrainSeconds:                 p.DrainSeconds,
		ArmedAt:                      armedAt.UTC(),
		ObservedRouteDigest:          p.ObservedRouteDigest,
		Phase:                        DrainPending,
	}, true
}

func (d PendingDrain) validate() error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidDrain, msg) }
	switch {
	case d.AttemptID <= 0:
		return bad("attempt id must be positive")
	case !github.ValidOpaqueID(d.EnvironmentID):
		return bad("invalid environment id")
	case !github.ValidOpaqueID(d.ReleaseID):
		return bad("invalid release id")
	case !validRuntimeInstanceID(d.BackendRuntimeInstanceID):
		return bad("invalid backend runtime instance id")
	case !github.ValidOpaqueID(d.PredecessorReleaseID):
		return bad("invalid predecessor release id")
	case !validRuntimeInstanceID(d.PredecessorRuntimeInstanceID):
		return bad("invalid predecessor runtime instance id")
	case d.PredecessorRuntimeInstanceID == d.BackendRuntimeInstanceID:
		return bad("stop target runtime instance equals the new runtime instance")
	case d.PredecessorReleaseID == d.ReleaseID:
		return bad("stop target release equals the new release")
	case d.DrainSeconds < 1 || d.DrainSeconds > maxDrainSeconds:
		return bad("drain seconds must be 1..300")
	case d.ArmedAt.IsZero():
		return bad("armed_at required")
	case d.ObservedRouteDigest == [32]byte{}:
		return bad("observed route digest required")
	case d.Attempts < 0:
		return bad("attempts must not be negative")
	}
	switch d.Phase {
	case DrainPending:
		if d.FailureSummary != "" {
			return bad("pending drain must not carry a failure summary")
		}
		if d.Retryable {
			return bad("pending drain must not be marked retryable")
		}
	case DrainStopFailed:
		if d.FailureSummary == "" || len(d.FailureSummary) > maxDrainSummaryLn {
			return bad("failure summary must be 1..2048 bytes")
		}
		if !utf8.ValidString(d.FailureSummary) {
			return bad("failure summary must be valid UTF-8")
		}
	default:
		return bad("unknown phase")
	}
	return nil
}

// drainWire is the on-disk JSON shape; the digest is lowercase hex.
type drainWire struct {
	AttemptID                    int64     `json:"attempt_id"`
	EnvironmentID                string    `json:"environment_id"`
	ReleaseID                    string    `json:"release_id"`
	BackendRuntimeInstanceID     string    `json:"backend_runtime_instance_id"`
	PredecessorReleaseID         string    `json:"predecessor_release_id"`
	PredecessorRuntimeInstanceID string    `json:"predecessor_runtime_instance_id"`
	DrainSeconds                 int       `json:"drain_seconds"`
	ArmedAt                      time.Time `json:"armed_at"`
	ObservedRouteDigest          string    `json:"observed_route_digest"`
	Phase                        string    `json:"phase"`
	FailureSummary               string    `json:"failure_summary"`
	Attempts                     int       `json:"attempts"`
	Retryable                    bool      `json:"retryable"`
	CreatedAt                    time.Time `json:"created_at"`
	UpdatedAt                    time.Time `json:"updated_at"`
}

func (d PendingDrain) marshal() ([]byte, error) {
	return json.Marshal(drainWire{
		AttemptID: d.AttemptID, EnvironmentID: d.EnvironmentID,
		ReleaseID: d.ReleaseID, BackendRuntimeInstanceID: d.BackendRuntimeInstanceID,
		PredecessorReleaseID:         d.PredecessorReleaseID,
		PredecessorRuntimeInstanceID: d.PredecessorRuntimeInstanceID,
		DrainSeconds:                 d.DrainSeconds, ArmedAt: d.ArmedAt,
		ObservedRouteDigest: hexDigest(d.ObservedRouteDigest),
		Phase:               string(d.Phase), FailureSummary: d.FailureSummary,
		Attempts: d.Attempts, Retryable: d.Retryable, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
	})
}

func unmarshalDrain(contents []byte) (PendingDrain, error) {
	var w drainWire
	if err := decodeStrict(contents, &w); err != nil {
		return PendingDrain{}, err
	}
	observed, ok := parseHexDigest(w.ObservedRouteDigest)
	if !ok {
		return PendingDrain{}, errors.New("malformed digest")
	}
	d := PendingDrain{
		AttemptID: w.AttemptID, EnvironmentID: w.EnvironmentID,
		ReleaseID: w.ReleaseID, BackendRuntimeInstanceID: w.BackendRuntimeInstanceID,
		PredecessorReleaseID:         w.PredecessorReleaseID,
		PredecessorRuntimeInstanceID: w.PredecessorRuntimeInstanceID,
		DrainSeconds:                 w.DrainSeconds, ArmedAt: w.ArmedAt.UTC(),
		ObservedRouteDigest: observed, Phase: DrainPhase(w.Phase),
		FailureSummary: w.FailureSummary, Attempts: w.Attempts, Retryable: w.Retryable,
		CreatedAt: w.CreatedAt.UTC(), UpdatedAt: w.UpdatedAt.UTC(),
	}
	if err := d.validate(); err != nil {
		return PendingDrain{}, err
	}
	if d.CreatedAt.IsZero() || d.UpdatedAt.IsZero() {
		return PendingDrain{}, errors.New("missing timestamp")
	}
	return d, nil
}

// sameTarget reports whether two records agree on every field fixed at arming
// (ArmedAt is excluded: it is preserved, never compared).
func (d PendingDrain) sameTarget(o PendingDrain) bool {
	return d.AttemptID == o.AttemptID && d.EnvironmentID == o.EnvironmentID &&
		d.ReleaseID == o.ReleaseID && d.BackendRuntimeInstanceID == o.BackendRuntimeInstanceID &&
		d.PredecessorReleaseID == o.PredecessorReleaseID &&
		d.PredecessorRuntimeInstanceID == o.PredecessorRuntimeInstanceID &&
		d.DrainSeconds == o.DrainSeconds && d.ObservedRouteDigest == o.ObservedRouteDigest
}

// DrainStore persists one PendingDrain file per attempt under <stateDir>/drains.
// Same exclusion rules as ActivationStore (in-process, per directory). The zero
// value is unusable and its methods return errors.
type DrainStore struct {
	dir string
	now func() time.Time
}

// Drains returns the pending-drain store rooted at <stateDir>/drains, creating
// the directory (0700) on first use.
func (s Store) Drains() (DrainStore, error) {
	if s.dir == "" {
		return DrainStore{}, errUninitialized
	}
	dir := filepath.Join(s.dir, drainsDirName)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return DrainStore{}, fmt.Errorf("create pending drain directory: %w", err)
	}
	if err := syncDirAndParent(dir); err != nil {
		return DrainStore{}, fmt.Errorf("sync pending drain directory: %w", err)
	}
	return DrainStore{dir: dir, now: time.Now}, nil
}

func drainFileName(attemptID int64) string {
	return drainFilePrefix + strconv.FormatInt(attemptID, 10) + drainFileSuffix
}

func (d DrainStore) path(attemptID int64) string {
	return filepath.Join(d.dir, drainFileName(attemptID))
}

func parseDrainFileName(name string) (int64, bool) {
	if !strings.HasPrefix(name, drainFilePrefix) || !strings.HasSuffix(name, drainFileSuffix) {
		return 0, false
	}
	n, err := strconv.ParseInt(name[len(drainFilePrefix):len(name)-len(drainFileSuffix)], 10, 64)
	if err != nil || n <= 0 || drainFileName(n) != name {
		return 0, false
	}
	return n, true
}

func (d DrainStore) readFile(attemptID int64) (PendingDrain, bool, error) {
	contents, err := os.ReadFile(d.path(attemptID))
	if errors.Is(err, os.ErrNotExist) {
		return PendingDrain{}, false, nil
	}
	if err != nil {
		return PendingDrain{}, false, corrupt(err)
	}
	rec, err := unmarshalDrain(contents)
	if err != nil {
		return PendingDrain{}, false, corrupt(err)
	}
	if rec.AttemptID != attemptID {
		return PendingDrain{}, false, corrupt(errors.New("file name does not match record attempt id"))
	}
	return rec, true, nil
}

// SaveDrain validates and atomically writes p. Over an existing record it
// refuses any change to a field fixed at arming (ErrDrainImmutable), lowering
// Attempts, or stop_failed -> pending without a higher Attempts
// (ErrDrainRegressed). ArmedAt and CreatedAt are preserved from an existing
// record (re-arming after a crash must not restart the drain clock); UpdatedAt
// is always now, in UTC. Saving a record identical to the stored one is a
// no-op success.
func (d DrainStore) SaveDrain(p PendingDrain) error {
	return d.save(p, false)
}

// ArmDrain creates the obligation for p (phase pending, Attempts 0) or, if a
// record for the attempt already exists with the same target, succeeds without
// touching it: the drainer may already have progressed it, and a re-arm after
// a crash must never rewind that. A differing target is ErrDrainImmutable.
func (d DrainStore) ArmDrain(p PendingDrain) error {
	if p.Phase != DrainPending || p.Attempts != 0 {
		return fmt.Errorf("%w: arming requires phase pending and zero attempts", ErrInvalidDrain)
	}
	return d.save(p, true)
}

func (d DrainStore) save(p PendingDrain, arm bool) error {
	if d.dir == "" {
		return errUninitialized
	}
	if err := p.validate(); err != nil {
		return err
	}
	mu := lockDir(d.dir)
	mu.Lock()
	defer mu.Unlock()
	now := d.now().UTC()
	prev, existed, err := d.readFile(p.AttemptID)
	if err != nil {
		return err
	}
	if existed {
		if !p.sameTarget(prev) {
			return ErrDrainImmutable
		}
		if arm {
			return nil
		}
		if p.Attempts < prev.Attempts || (prev.Phase == DrainStopFailed && p.Phase == DrainPending && p.Attempts <= prev.Attempts) {
			return ErrDrainRegressed
		}
		p.ArmedAt = prev.ArmedAt
		p.CreatedAt = prev.CreatedAt
		if p.Phase == prev.Phase && p.FailureSummary == prev.FailureSummary && p.Attempts == prev.Attempts && p.Retryable == prev.Retryable {
			return nil
		}
	} else {
		p.CreatedAt = now
	}
	p.ArmedAt = p.ArmedAt.UTC()
	p.UpdatedAt = now
	contents, err := p.marshal()
	if err != nil {
		return err
	}
	return writeFileAtomic(d.dir, d.path(p.AttemptID), contents)
}

// LoadDrain returns the record for attemptID. existed=false with a nil error
// means no record; an untrustworthy file is ErrCorruptState.
func (d DrainStore) LoadDrain(attemptID int64) (PendingDrain, bool, error) {
	if attemptID <= 0 {
		return PendingDrain{}, false, fmt.Errorf("%w: attempt id must be positive", ErrInvalidDrain)
	}
	if d.dir == "" {
		return PendingDrain{}, false, errUninitialized
	}
	mu := lockDir(d.dir)
	mu.Lock()
	defer mu.Unlock()
	return d.readFile(attemptID)
}

// ScanDrains returns every valid record sorted by attempt id and, separately,
// the names of entries that are not valid records (never modified or deleted).
// Atomic temp files are skipped. err is only for a directory-level failure.
func (d DrainStore) ScanDrains() (records []PendingDrain, corruptNames []string, err error) {
	if d.dir == "" {
		return nil, nil, errUninitialized
	}
	mu := lockDir(d.dir)
	mu.Lock()
	defer mu.Unlock()
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return nil, nil, corrupt(err)
	}
	for _, e := range entries {
		if isAtomicTemp(e.Name()) && e.Type().IsRegular() {
			continue
		}
		id, ok := parseDrainFileName(e.Name())
		if !ok || !e.Type().IsRegular() {
			corruptNames = append(corruptNames, e.Name())
			continue
		}
		rec, existed, rerr := d.readFile(id)
		if rerr != nil || !existed {
			corruptNames = append(corruptNames, e.Name())
			continue
		}
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].AttemptID < records[j].AttemptID })
	return records, corruptNames, nil
}

// DeleteDrain removes the record for attemptID. Idempotent; fsyncs the
// directory so the removal is durable.
func (d DrainStore) DeleteDrain(attemptID int64) error {
	if attemptID <= 0 {
		return fmt.Errorf("%w: attempt id must be positive", ErrInvalidDrain)
	}
	if d.dir == "" {
		return errUninitialized
	}
	mu := lockDir(d.dir)
	mu.Lock()
	defer mu.Unlock()
	if err := os.Remove(d.path(attemptID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(d.dir)
}
