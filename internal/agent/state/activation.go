package state

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"autodeploy/internal/github"
	"autodeploy/internal/runtime/dockerid"
)

// Pending activation records (tasks/10d-plan.md, 10d-3). A record is written
// BEFORE the agent touches the Traefik route and deleted only after the
// control plane gives a terminal answer to the result report, so a crash
// between switching traffic and reporting can never lose the report.

const (
	activationsDirName     = "activations"
	activationFilePrefix   = "attempt-"
	activationFileSuffix   = ".json"
	maxFailureSummaryBytes = 2048
	// atomicTempPrefix/Suffix are the os.CreateTemp pattern used by
	// writeFileAtomic; the List skip rule uses the same constants.
	atomicTempPrefix        = ".fence-"
	atomicTempSuffix        = ".tmp"
	dirPerm                 = 0700
	routeRevisionsDirName   = "route-revisions"
	routeRevisionFileSuffix = ".json"
)

var (
	// ErrActivationRegressed is returned when a save would move a record from
	// result_ready back to authorized: a result must never regress.
	ErrActivationRegressed = errors.New("pending activation result must not regress")
	// ErrActivationImmutable is returned when a save would change a field that
	// is fixed once the attempt was authorized.
	ErrActivationImmutable = errors.New("pending activation immutable field changed")
	// ErrInvalidActivation wraps every validation failure of a record.
	ErrInvalidActivation = errors.New("invalid pending activation")
)

// ActivationPhase is how far an activation attempt has progressed locally.
type ActivationPhase string

const (
	// PhaseAuthorized: the server authorized the switch; the route may or may
	// not have been written yet.
	PhaseAuthorized ActivationPhase = "authorized"
	// PhaseResultReady: the outcome is known and must be reported.
	PhaseResultReady ActivationPhase = "result_ready"
)

// PendingActivation is one durable, not-yet-acknowledged activation attempt.
type PendingActivation struct {
	AttemptID           int64
	Fence               int64
	EnvironmentID       string
	OperationID         string // server-issued decimal int64
	RouteDigest         [32]byte
	ImageDigest         [32]byte
	RouteRevision       int64
	Phase               ActivationPhase
	Succeeded           bool     // result_ready only
	ObservedRouteDigest [32]byte // result_ready && Succeeded
	FailureSummary      string   // result_ready && !Succeeded
	CreatedAt           time.Time
	UpdatedAt           time.Time

	// Drain obligation inputs (tasks/10e-plan.md, 10e-3). All immutable. Records
	// written before 10e lack them and decode to zero values, meaning "no
	// predecessor, nothing to drain".
	ReleaseID                    string // the new release, from the lease
	BackendRuntimeInstanceID     string // the new runtime instance, from the activate response
	PredecessorReleaseID         string // both predecessor ids empty, or both set
	PredecessorRuntimeInstanceID string
	DrainSeconds                 int // from the lease; 1..maxDrainSeconds when a predecessor is set
}

// maxDrainSeconds mirrors the server's frozen drain_seconds bound (1-300).
const maxDrainSeconds = 300

// HasPredecessor reports whether the activation replaced a running release.
func (p PendingActivation) HasPredecessor() bool { return p.PredecessorRuntimeInstanceID != "" }

func validRuntimeInstanceID(s string) bool {
	_, err := dockerid.ContainerName(s)
	return err == nil
}

func (p PendingActivation) validateDrainFields() error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidActivation, msg) }
	if p.PredecessorReleaseID == "" && p.PredecessorRuntimeInstanceID == "" {
		// No predecessor: nothing will be drained, so the new-release fields
		// stay as lenient as before 10e (an odd first-activation lease must
		// not fail the authorized save).
		return nil
	}
	if p.ReleaseID != "" && !github.ValidOpaqueID(p.ReleaseID) {
		return bad("invalid release id")
	}
	if p.BackendRuntimeInstanceID != "" && !validRuntimeInstanceID(p.BackendRuntimeInstanceID) {
		return bad("invalid backend runtime instance id")
	}
	if p.DrainSeconds < 0 || p.DrainSeconds > maxDrainSeconds {
		return bad("drain seconds out of range")
	}
	switch {
	case p.PredecessorReleaseID == "" || p.PredecessorRuntimeInstanceID == "":
		return bad("predecessor release and runtime instance ids must be set together")
	case !github.ValidOpaqueID(p.PredecessorReleaseID):
		return bad("invalid predecessor release id")
	case !validRuntimeInstanceID(p.PredecessorRuntimeInstanceID):
		return bad("invalid predecessor runtime instance id")
	case p.ReleaseID == "" || p.BackendRuntimeInstanceID == "":
		return bad("a predecessor requires the new release and backend runtime instance ids")
	case p.PredecessorRuntimeInstanceID == p.BackendRuntimeInstanceID:
		return bad("predecessor runtime instance must differ from the backend runtime instance")
	case p.PredecessorReleaseID == p.ReleaseID:
		return bad("predecessor release must differ from the release")
	case p.DrainSeconds < 1:
		return bad("drain seconds must be 1..300 when a predecessor is set")
	}
	return nil
}

func (p PendingActivation) validate() error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidActivation, msg) }
	switch {
	case p.AttemptID <= 0:
		return bad("attempt id must be positive")
	case p.Fence <= 0:
		return bad("fence must be positive")
	case !github.ValidOpaqueID(p.EnvironmentID):
		return bad("invalid environment id")
	case !validOperationID(p.OperationID):
		return bad("operation id must be a canonical positive decimal int64")
	case p.RouteDigest == [32]byte{}:
		return bad("route digest required")
	case p.ImageDigest == [32]byte{}:
		return bad("image digest required")
	case p.RouteRevision <= 0:
		return bad("route revision must be positive")
	}
	if err := p.validateDrainFields(); err != nil {
		return err
	}
	switch p.Phase {
	case PhaseAuthorized:
		if p.Succeeded || p.ObservedRouteDigest != [32]byte{} || p.FailureSummary != "" {
			return bad("authorized record must carry no result")
		}
	case PhaseResultReady:
		if p.Succeeded {
			if p.ObservedRouteDigest == [32]byte{} {
				return bad("successful result requires observed route digest")
			}
			if p.FailureSummary != "" {
				return bad("successful result must not carry a failure summary")
			}
		} else {
			if p.ObservedRouteDigest != [32]byte{} {
				return bad("failed result must not carry an observed route digest")
			}
			if p.FailureSummary == "" || len(p.FailureSummary) > maxFailureSummaryBytes {
				return bad("failure summary must be 1..2048 bytes")
			}
			// json.Marshal rewrites invalid bytes to U+FFFD (3 bytes), which
			// would make a stored record fail validation on reload.
			if !utf8.ValidString(p.FailureSummary) {
				return bad("failure summary must be valid UTF-8")
			}
		}
	default:
		return bad("unknown phase")
	}
	return nil
}

func validOperationID(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == s
}

// activationWire is the on-disk JSON shape; digests are lowercase hex.
type activationWire struct {
	AttemptID           int64     `json:"attempt_id"`
	Fence               int64     `json:"fence"`
	EnvironmentID       string    `json:"environment_id"`
	OperationID         string    `json:"operation_id"`
	RouteDigest         string    `json:"route_digest"`
	ImageDigest         string    `json:"image_digest"`
	RouteRevision       int64     `json:"route_revision"`
	Phase               string    `json:"phase"`
	Succeeded           bool      `json:"succeeded"`
	ObservedRouteDigest string    `json:"observed_route_digest"`
	FailureSummary      string    `json:"failure_summary"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`

	// Absent in records written before 10e: they decode to zero values.
	ReleaseID                    string `json:"release_id"`
	BackendRuntimeInstanceID     string `json:"backend_runtime_instance_id"`
	PredecessorReleaseID         string `json:"predecessor_release_id"`
	PredecessorRuntimeInstanceID string `json:"predecessor_runtime_instance_id"`
	DrainSeconds                 int    `json:"drain_seconds"`
}

func hexDigest(d [32]byte) string { return hex.EncodeToString(d[:]) }

func parseHexDigest(s string) ([32]byte, bool) {
	var d [32]byte
	if len(s) != 64 || s != strings.ToLower(s) {
		return d, false
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return d, false
	}
	copy(d[:], b)
	return d, true
}

func (p PendingActivation) marshal() ([]byte, error) {
	return json.Marshal(activationWire{
		AttemptID: p.AttemptID, Fence: p.Fence, EnvironmentID: p.EnvironmentID,
		OperationID: p.OperationID, RouteDigest: hexDigest(p.RouteDigest),
		ImageDigest: hexDigest(p.ImageDigest), RouteRevision: p.RouteRevision,
		Phase: string(p.Phase), Succeeded: p.Succeeded,
		ObservedRouteDigest: hexDigest(p.ObservedRouteDigest),
		FailureSummary:      p.FailureSummary,
		CreatedAt:           p.CreatedAt, UpdatedAt: p.UpdatedAt,
		ReleaseID: p.ReleaseID, BackendRuntimeInstanceID: p.BackendRuntimeInstanceID,
		PredecessorReleaseID:         p.PredecessorReleaseID,
		PredecessorRuntimeInstanceID: p.PredecessorRuntimeInstanceID,
		DrainSeconds:                 p.DrainSeconds,
	})
}

// decodeStrict decodes exactly one JSON value, rejecting unknown fields and
// trailing data.
func decodeStrict(contents []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(contents))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

func unmarshalActivation(contents []byte) (PendingActivation, error) {
	var w activationWire
	if err := decodeStrict(contents, &w); err != nil {
		return PendingActivation{}, err
	}
	route, ok1 := parseHexDigest(w.RouteDigest)
	image, ok2 := parseHexDigest(w.ImageDigest)
	observed, ok3 := parseHexDigest(w.ObservedRouteDigest)
	if !ok1 || !ok2 || !ok3 {
		return PendingActivation{}, errors.New("malformed digest")
	}
	p := PendingActivation{
		AttemptID: w.AttemptID, Fence: w.Fence, EnvironmentID: w.EnvironmentID,
		OperationID: w.OperationID, RouteDigest: route, ImageDigest: image,
		RouteRevision: w.RouteRevision, Phase: ActivationPhase(w.Phase),
		Succeeded: w.Succeeded, ObservedRouteDigest: observed,
		FailureSummary: w.FailureSummary,
		CreatedAt:      w.CreatedAt.UTC(), UpdatedAt: w.UpdatedAt.UTC(),
		ReleaseID: w.ReleaseID, BackendRuntimeInstanceID: w.BackendRuntimeInstanceID,
		PredecessorReleaseID:         w.PredecessorReleaseID,
		PredecessorRuntimeInstanceID: w.PredecessorRuntimeInstanceID,
		DrainSeconds:                 w.DrainSeconds,
	}
	if err := p.validate(); err != nil {
		return PendingActivation{}, err
	}
	if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
		return PendingActivation{}, errors.New("missing timestamp")
	}
	return p, nil
}

// ActivationStore persists one PendingActivation file per attempt. One agent
// process owns a state dir: exclusion between store instances is in-process
// only (a package-level lock keyed by directory); cross-process exclusion is
// not provided. The zero value is unusable and its methods return errors.
type ActivationStore struct {
	dir string
	now func() time.Time
}

var errUninitialized = errors.New("state store not initialized")

var dirLocks sync.Map // cleaned directory path -> *sync.Mutex

// lockDir returns the lock shared by every store instance over dir.
func lockDir(dir string) *sync.Mutex {
	m, _ := dirLocks.LoadOrStore(filepath.Clean(dir), &sync.Mutex{})
	return m.(*sync.Mutex)
}

// corrupt flattens cause into ErrCorruptState while keeping it visible.
func corrupt(cause error) error { return fmt.Errorf("%w: %v", ErrCorruptState, cause) }

// syncDirAndParent fsyncs dir and the directory containing it, making the
// dir's own entry durable.
func syncDirAndParent(dir string) error {
	if err := syncDir(dir); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dir))
}

// Activations returns the pending-activation store rooted at
// <stateDir>/activations, creating the directory (0700) on first use.
func (s Store) Activations() (ActivationStore, error) {
	if s.dir == "" {
		return ActivationStore{}, errUninitialized
	}
	dir := filepath.Join(s.dir, activationsDirName)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return ActivationStore{}, fmt.Errorf("create pending activation directory: %w", err)
	}
	if err := syncDirAndParent(dir); err != nil {
		return ActivationStore{}, fmt.Errorf("sync pending activation directory: %w", err)
	}
	return ActivationStore{dir: dir, now: time.Now}, nil
}

func (a ActivationStore) path(attemptID int64) string {
	return filepath.Join(a.dir, activationFileName(attemptID))
}

func activationFileName(attemptID int64) string {
	return activationFilePrefix + strconv.FormatInt(attemptID, 10) + activationFileSuffix
}

// parseActivationFileName returns the attempt id encoded in a record file
// name, requiring the canonical form produced by activationFileName.
func parseActivationFileName(name string) (int64, bool) {
	if !strings.HasPrefix(name, activationFilePrefix) || !strings.HasSuffix(name, activationFileSuffix) {
		return 0, false
	}
	n, err := strconv.ParseInt(name[len(activationFilePrefix):len(name)-len(activationFileSuffix)], 10, 64)
	if err != nil || n <= 0 || activationFileName(n) != name {
		return 0, false
	}
	return n, true
}

// isAtomicTemp reports whether name is a leftover temp file of
// writeFileAtomic. That helper creates "<dir>/.fence-<random>.tmp" via
// os.CreateTemp and renames it over the target; a crash can leave one
// behind. A record file is never named that way (records are
// "attempt-<n>.json"), so a leading ".fence-" plus trailing ".tmp" is
// unambiguous and the only thing List ignores. A temp file is never a
// committed record, so ignoring it cannot lose a report.
func isAtomicTemp(name string) bool {
	return strings.HasPrefix(name, atomicTempPrefix) && strings.HasSuffix(name, atomicTempSuffix)
}

func (a ActivationStore) readFile(attemptID int64) (PendingActivation, bool, error) {
	contents, err := os.ReadFile(a.path(attemptID))
	if errors.Is(err, os.ErrNotExist) {
		return PendingActivation{}, false, nil
	}
	if err != nil {
		return PendingActivation{}, false, corrupt(err)
	}
	p, err := unmarshalActivation(contents)
	if err != nil {
		return PendingActivation{}, false, corrupt(err)
	}
	if p.AttemptID != attemptID {
		return PendingActivation{}, false, corrupt(errors.New("file name does not match record attempt id"))
	}
	return p, true, nil
}

// SaveActivation validates and atomically writes p. Refused: an authorized
// record over an existing result_ready one (ErrActivationRegressed) and any
// change to an immutable field of an existing record
// (ErrActivationImmutable), including any change to the result of a
// result_ready record (an identical rewrite is allowed). CreatedAt is preserved from an existing record
// (else the caller's value, else now); UpdatedAt is always now, in UTC.
func (a ActivationStore) SaveActivation(p PendingActivation) error {
	if a.dir == "" {
		return errUninitialized
	}
	if err := p.validate(); err != nil {
		return err
	}
	mu := lockDir(a.dir)
	mu.Lock()
	defer mu.Unlock()
	now := a.now().UTC()
	prev, existed, err := a.readFile(p.AttemptID)
	if err != nil {
		return err
	}
	if existed {
		if prev.Fence != p.Fence || prev.EnvironmentID != p.EnvironmentID ||
			prev.OperationID != p.OperationID || prev.RouteDigest != p.RouteDigest ||
			prev.ImageDigest != p.ImageDigest || prev.RouteRevision != p.RouteRevision ||
			prev.ReleaseID != p.ReleaseID || prev.BackendRuntimeInstanceID != p.BackendRuntimeInstanceID ||
			prev.PredecessorReleaseID != p.PredecessorReleaseID ||
			prev.PredecessorRuntimeInstanceID != p.PredecessorRuntimeInstanceID ||
			prev.DrainSeconds != p.DrainSeconds {
			return ErrActivationImmutable
		}
		if prev.Phase == PhaseResultReady && p.Phase == PhaseAuthorized {
			return ErrActivationRegressed
		}
		// A recorded result is immutable: the server accepts a re-sent
		// report as a replay only when identical to the first.
		if prev.Phase == PhaseResultReady && (prev.Succeeded != p.Succeeded ||
			prev.ObservedRouteDigest != p.ObservedRouteDigest ||
			prev.FailureSummary != p.FailureSummary) {
			return ErrActivationImmutable
		}
		p.CreatedAt = prev.CreatedAt
	} else if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.CreatedAt = p.CreatedAt.UTC()
	p.UpdatedAt = now
	contents, err := p.marshal()
	if err != nil {
		return err
	}
	return writeFileAtomic(a.dir, a.path(p.AttemptID), contents)
}

// LoadActivation returns the record for attemptID. existed=false with a nil
// error means no record; a present but untrustworthy file is ErrCorruptState.
func (a ActivationStore) LoadActivation(attemptID int64) (PendingActivation, bool, error) {
	if attemptID <= 0 {
		return PendingActivation{}, false, fmt.Errorf("%w: attempt id must be positive", ErrInvalidActivation)
	}
	if a.dir == "" {
		return PendingActivation{}, false, errUninitialized
	}
	mu := lockDir(a.dir)
	mu.Lock()
	defer mu.Unlock()
	return a.readFile(attemptID)
}

// ListActivations returns every pending record sorted by AttemptID.
// writeFileAtomic temp files (see isAtomicTemp) are skipped. It otherwise
// fails closed: any other entry that is not a valid record yields
// ErrCorruptState rather than being skipped, because a skipped record is a
// silently lost report.
func (a ActivationStore) ListActivations() ([]PendingActivation, error) {
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
	out := make([]PendingActivation, 0, len(entries))
	for _, e := range entries {
		if isAtomicTemp(e.Name()) && e.Type().IsRegular() {
			continue
		}
		id, ok := parseActivationFileName(e.Name())
		if !ok || !e.Type().IsRegular() {
			return nil, corrupt(fmt.Errorf("unexpected entry %q in pending activation directory", e.Name()))
		}
		p, existed, err := a.readFile(id)
		if err != nil {
			return nil, err
		}
		if !existed {
			return nil, corrupt(fmt.Errorf("record %q vanished during listing", e.Name()))
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AttemptID < out[j].AttemptID })
	return out, nil
}

// ScanActivations is the per-file counterpart of ListActivations for the
// reporter: one unreadable entry must not block delivery of the others. Valid
// records are returned sorted by attempt id; every entry that would make
// ListActivations fail (bad JSON, invalid record, name/content mismatch, stray
// file, non-regular entry, non-canonical name) is returned by name in corrupt
// and is never modified or deleted. Atomic temp files are skipped. err is only
// for a directory-level failure.
func (a ActivationStore) ScanActivations() (records []PendingActivation, corruptNames []string, err error) {
	if a.dir == "" {
		return nil, nil, errUninitialized
	}
	mu := lockDir(a.dir)
	mu.Lock()
	defer mu.Unlock()
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return nil, nil, corrupt(err)
	}
	for _, e := range entries {
		if isAtomicTemp(e.Name()) && e.Type().IsRegular() {
			continue
		}
		id, ok := parseActivationFileName(e.Name())
		if !ok || !e.Type().IsRegular() {
			corruptNames = append(corruptNames, e.Name())
			continue
		}
		p, existed, rerr := a.readFile(id)
		if rerr != nil || !existed {
			corruptNames = append(corruptNames, e.Name())
			continue
		}
		records = append(records, p)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].AttemptID < records[j].AttemptID })
	return records, corruptNames, nil
}

// DeleteActivation removes the record for attemptID. It is idempotent and
// fsyncs the directory so the removal is durable (a resurrected record only
// causes an idempotent re-report, but a durable delete avoids the churn).
func (a ActivationStore) DeleteActivation(attemptID int64) error {
	if attemptID <= 0 {
		return fmt.Errorf("%w: attempt id must be positive", ErrInvalidActivation)
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

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
