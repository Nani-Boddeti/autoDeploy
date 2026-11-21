package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"autodeploy/internal/github"
)

// Durable attempt records (tasks/23-plan.md, 23-G). One record per attempt the
// agent is executing, written before any phase runs and advanced BEFORE each
// phase begins, so a record never claims an earlier phase than the work in
// progress. A restarted agent reads leftovers to learn which attempts it lost
// and how far they got. The record is only a progress marker: it never says an
// activation outcome (owner decision D-B); activation recovery owns that via
// the pending activation records. A record at claimed, fetching or building
// means no traffic-affecting state exists (no container was created or
// started); build cache and images (cleaned by retention), the checkout
// workspace (reclaimed by the quota service) and the shared BuildKit builder
// may remain and are harmless to traffic.

const (
	attemptsDirName       = "attempts"
	attemptRecordVersion  = 1
	maxAttemptPhaseLength = 32
)

// AttemptPhaseClaimed is the phase of a freshly claimed lease: no phase has
// started yet.
const AttemptPhaseClaimed = "claimed"

// ErrInvalidAttempt wraps every validation failure of an attempt record.
var ErrInvalidAttempt = errors.New("invalid attempt record")

// AttemptRecord is one durable in-flight attempt.
type AttemptRecord struct {
	AttemptID     int64
	JobID         int64
	DeploymentID  string
	EnvironmentID string
	Fence         int64
	// Phase is the phase about to begin (or in progress): "claimed", or a
	// deployment status such as "fetching", "building", "starting".
	Phase     string
	UpdatedAt time.Time
}

type attemptWire struct {
	Version       int       `json:"version"`
	AttemptID     int64     `json:"attempt_id"`
	JobID         int64     `json:"job_id"`
	DeploymentID  string    `json:"deployment_id"`
	EnvironmentID string    `json:"environment_id"`
	Fence         int64     `json:"fence"`
	Phase         string    `json:"phase"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func validAttemptPhase(s string) bool {
	if s == "" || len(s) > maxAttemptPhaseLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 'a' || c > 'z' {
			if c != '_' {
				return false
			}
		}
	}
	return true
}

func (r AttemptRecord) validate() error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidAttempt, msg) }
	switch {
	case r.AttemptID <= 0:
		return bad("attempt id must be positive")
	case r.Fence <= 0:
		return bad("fence must be positive")
	case r.JobID < 0:
		return bad("job id must not be negative")
	case !github.ValidOpaqueID(r.EnvironmentID):
		return bad("invalid environment id")
	case r.DeploymentID != "" && !github.ValidOpaqueID(r.DeploymentID):
		return bad("invalid deployment id")
	case !validAttemptPhase(r.Phase):
		return bad("invalid phase")
	}
	return nil
}

func unmarshalAttempt(contents []byte) (AttemptRecord, error) {
	var w attemptWire
	if err := decodeStrict(contents, &w); err != nil {
		return AttemptRecord{}, err
	}
	if w.Version != attemptRecordVersion {
		return AttemptRecord{}, fmt.Errorf("unsupported attempt record version %d", w.Version)
	}
	r := AttemptRecord{
		AttemptID: w.AttemptID, JobID: w.JobID, DeploymentID: w.DeploymentID,
		EnvironmentID: w.EnvironmentID, Fence: w.Fence, Phase: w.Phase, UpdatedAt: w.UpdatedAt,
	}
	if err := r.validate(); err != nil {
		return AttemptRecord{}, err
	}
	return r, nil
}

// AttemptStore persists attempt records under <stateDir>/attempts, a
// subdirectory so the fence store's "any *.json at the root" check never sees
// them. Like ActivationStore, exclusion between instances is in-process only.
// The zero value is unusable and its methods return errors.
type AttemptStore struct {
	dir string
	now func() time.Time
}

// Attempts returns the attempt-record store rooted at <stateDir>/attempts,
// creating the directory (0700) on first use.
func (s Store) Attempts() (AttemptStore, error) {
	if s.dir == "" {
		return AttemptStore{}, errUninitialized
	}
	dir := filepath.Join(s.dir, attemptsDirName)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return AttemptStore{}, fmt.Errorf("create attempt record directory: %w", err)
	}
	if err := syncDirAndParent(dir); err != nil {
		return AttemptStore{}, fmt.Errorf("sync attempt record directory: %w", err)
	}
	return AttemptStore{dir: dir, now: time.Now}, nil
}

func (a AttemptStore) path(attemptID int64) string {
	return filepath.Join(a.dir, activationFileName(attemptID))
}

func (a AttemptStore) readFile(attemptID int64) (AttemptRecord, bool, error) {
	contents, err := os.ReadFile(a.path(attemptID))
	if errors.Is(err, os.ErrNotExist) {
		return AttemptRecord{}, false, nil
	}
	if err != nil {
		return AttemptRecord{}, false, corrupt(err)
	}
	r, err := unmarshalAttempt(contents)
	if err != nil {
		return AttemptRecord{}, false, corrupt(err)
	}
	if r.AttemptID != attemptID {
		return AttemptRecord{}, false, corrupt(errors.New("file name does not match record attempt id"))
	}
	return r, true, nil
}

func (a AttemptStore) write(r AttemptRecord) error {
	r.UpdatedAt = a.now().UTC()
	contents, err := json.Marshal(attemptWire{
		Version: attemptRecordVersion, AttemptID: r.AttemptID, JobID: r.JobID, DeploymentID: r.DeploymentID,
		EnvironmentID: r.EnvironmentID, Fence: r.Fence, Phase: r.Phase, UpdatedAt: r.UpdatedAt,
	})
	if err != nil {
		return err
	}
	return writeFileAtomic(a.dir, a.path(r.AttemptID), contents)
}

// SaveAttempt validates and atomically writes r (UpdatedAt is set to now). An
// existing record's identity fields (job, deployment, environment, fence) may
// not change (ErrInvalidAttempt); an untrustworthy existing file is
// ErrCorruptState.
func (a AttemptStore) SaveAttempt(r AttemptRecord) error {
	if a.dir == "" {
		return errUninitialized
	}
	if err := r.validate(); err != nil {
		return err
	}
	mu := lockDir(a.dir)
	mu.Lock()
	defer mu.Unlock()
	prev, existed, err := a.readFile(r.AttemptID)
	if err != nil {
		return err
	}
	if existed && (prev.JobID != r.JobID || prev.DeploymentID != r.DeploymentID ||
		prev.EnvironmentID != r.EnvironmentID || prev.Fence != r.Fence) {
		return fmt.Errorf("%w: immutable field changed", ErrInvalidAttempt)
	}
	return a.write(r)
}

// SetAttemptPhase durably moves an existing record to phase. A missing record
// is an error: the phase is only ever advanced for an attempt whose record was
// written first.
func (a AttemptStore) SetAttemptPhase(attemptID int64, phase string) error {
	if a.dir == "" {
		return errUninitialized
	}
	if !validAttemptPhase(phase) {
		return fmt.Errorf("%w: invalid phase", ErrInvalidAttempt)
	}
	mu := lockDir(a.dir)
	mu.Lock()
	defer mu.Unlock()
	r, existed, err := a.readFile(attemptID)
	if err != nil {
		return err
	}
	if !existed {
		return fmt.Errorf("%w: no record for attempt %d", ErrInvalidAttempt, attemptID)
	}
	r.Phase = phase
	return a.write(r)
}

// LoadAttempt returns the record for attemptID. existed=false with a nil error
// means no record; a present but untrustworthy file is ErrCorruptState.
func (a AttemptStore) LoadAttempt(attemptID int64) (AttemptRecord, bool, error) {
	if attemptID <= 0 {
		return AttemptRecord{}, false, fmt.Errorf("%w: attempt id must be positive", ErrInvalidAttempt)
	}
	if a.dir == "" {
		return AttemptRecord{}, false, errUninitialized
	}
	mu := lockDir(a.dir)
	mu.Lock()
	defer mu.Unlock()
	return a.readFile(attemptID)
}

// ListAttempts returns every record sorted by AttemptID. writeFileAtomic temp
// files are skipped; any other entry that is not a valid record fails closed
// with ErrCorruptState, because a skipped record is a silently lost attempt.
func (a AttemptStore) ListAttempts() ([]AttemptRecord, error) {
	if a.dir == "" {
		return nil, errUninitialized
	}
	mu := lockDir(a.dir)
	mu.Lock()
	defer mu.Unlock()
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return nil, corrupt(err)
	}
	out := make([]AttemptRecord, 0, len(entries))
	for _, e := range entries {
		if isAtomicTemp(e.Name()) && e.Type().IsRegular() {
			continue
		}
		id, ok := parseActivationFileName(e.Name())
		if !ok || !e.Type().IsRegular() {
			return nil, corrupt(fmt.Errorf("unexpected entry %q in attempt record directory", e.Name()))
		}
		r, existed, err := a.readFile(id)
		if err != nil {
			return nil, err
		}
		if !existed {
			return nil, corrupt(fmt.Errorf("record %q vanished during listing", e.Name()))
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AttemptID < out[j].AttemptID })
	return out, nil
}

// DeleteAttempt removes the record for attemptID. It is idempotent and fsyncs
// the directory so the removal is durable.
func (a AttemptStore) DeleteAttempt(attemptID int64) error {
	if attemptID <= 0 {
		return fmt.Errorf("%w: attempt id must be positive", ErrInvalidAttempt)
	}
	if a.dir == "" {
		return errUninitialized
	}
	mu := lockDir(a.dir)
	mu.Lock()
	defer mu.Unlock()
	if err := os.Remove(a.path(attemptID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(a.dir)
}
