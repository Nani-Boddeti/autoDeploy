// Package state persists the host agent's per-environment fence state
// locally (docs/adr/0004 "Environment execution slots and fencing";
// tasks/todo.md Decision 5). The control-plane database is the sole
// authority for the environment fence counter, but the agent must never act
// on a claimed fence lower than the highest one it has already accepted --
// including across a restart -- so this state is durable, atomically
// written, and fails closed whenever it cannot prove that invariant.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"autodeploy/internal/github"
)

// ErrCorruptState reports a fence-state file that exists but cannot be
// trusted: unreadable, malformed, or internally inconsistent. Callers must
// fail closed and refuse to claim that environment until an operator or the
// control-plane reconcile flow clears it (ADR 0004: "lost or corrupt local
// fence state, the agent fails closed and reconciles before accepting
// work").
var ErrCorruptState = errors.New("corrupt agent fence state")

// Fence is one environment's durable local fence state.
// HighestAcceptedFence must never decrease; ReconciliationRequired blocks
// claiming that environment until the control-plane reconcile call clears
// it.
type Fence struct {
	EnvironmentID          string `json:"environment_id"`
	HighestAcceptedFence   int64  `json:"highest_accepted_fence"`
	ReconciliationRequired bool   `json:"reconciliation_required"`
}

func (f Fence) valid() bool {
	return github.ValidOpaqueID(f.EnvironmentID) && f.HighestAcceptedFence >= 0
}

// indexFileName is the durable manifest of every environment ID this store
// has ever accepted a fence for. It is a leading-dot name specifically so
// encodeName's output (alnum/-/_ or a leading "%") can never collide with
// it: an environment literally named "index" still encodes to a distinct
// on-disk file.
const indexFileName = ".fence-index.json"

// index is the on-disk shape of indexFileName.
type index struct {
	Environments []string `json:"environments"`
}

// Store persists one Fence file per environment inside dir, named by a
// filesystem-safe encoding of the environment ID so an opaque ID containing
// "/", "..", or other separators can never escape dir, plus indexFileName:
// a durable manifest of every environment ID ever recorded. The manifest
// exists because a missing per-environment file is otherwise
// indistinguishable between "this agent has never seen this environment"
// (safe to treat as zero state) and "this agent's fence state for an
// environment it already accepted work in was lost" (ADR 0004: fail
// closed). Consulting the manifest turns that ambiguity into an explicit
// answer instead of silently resetting a lost fence to zero.
type Store struct{ dir string }

// New creates (if needed) and returns a Store rooted at dir.
func New(dir string) (Store, error) {
	if dir == "" || !filepath.IsAbs(dir) {
		return Store{}, errors.New("invalid agent state directory")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Store{}, fmt.Errorf("create agent fence state directory: %w", err)
	}
	return Store{dir: dir}, nil
}

func (s Store) path(environmentID string) (string, error) {
	if !github.ValidOpaqueID(environmentID) {
		return "", errors.New("invalid environment id")
	}
	return filepath.Join(s.dir, encodeName(environmentID)+".json"), nil
}

// encodeName percent-encodes every byte outside a small safe allow-list so
// the resulting filename can never contain a path separator or NUL.
func encodeName(id string) string {
	var b strings.Builder
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02x", c)
	}
	return b.String()
}

// Load returns the durable fence state for environmentID, cross-checked
// against the durable index (see Store's doc comment) so a lost or
// tampered-with fence file can never be silently read back as zero state:
//
//   - Not listed in the index and no file present: a legitimate first
//     encounter with this environment. Returns the zero Fence,
//     existed=false, nil.
//   - Listed in the index but the file is missing, unreadable, malformed,
//     or names a different environment ID: the fence this agent once
//     accepted is lost or corrupt. ErrCorruptState; the caller must fail
//     closed and require reconciliation (ADR 0004).
//   - A file is present but its environment is not listed in the index:
//     also ErrCorruptState. This is never produced by an ordinary crash
//     (Save always writes the fence file before adding the index entry --
//     see Save's doc comment), so it can only mean the index itself was
//     lost/replaced or the directory was tampered with; either way the
//     agent cannot prove this fence is the one it last accepted.
//   - The index itself is missing or corrupt: ErrCorruptState for every
//     environment, UNLESS the state directory also contains no fence files
//     at all, which is exactly what a brand-new state directory looks
//     like and is treated as legitimate (empty index).
func (s Store) Load(environmentID string) (fence Fence, existed bool, err error) {
	path, err := s.path(environmentID)
	if err != nil {
		return Fence{}, false, err
	}
	indexed, failClosed := s.loadIndex()
	if failClosed {
		return Fence{}, false, ErrCorruptState
	}
	listed := indexed[environmentID]

	contents, readErr := os.ReadFile(path)
	switch {
	case errors.Is(readErr, os.ErrNotExist):
		if listed {
			return Fence{}, false, ErrCorruptState
		}
		return Fence{EnvironmentID: environmentID}, false, nil
	case readErr != nil:
		return Fence{}, false, ErrCorruptState
	case !listed:
		return Fence{}, false, ErrCorruptState
	}
	var f Fence
	if jsonErr := json.Unmarshal(contents, &f); jsonErr != nil || !f.valid() || f.EnvironmentID != environmentID {
		return Fence{}, false, ErrCorruptState
	}
	return f, true, nil
}

// Save durably persists fence, then records environmentID in the durable
// index. Write order is deliberate: the fence file is written and fsynced
// first, and only then is the index updated. A crash between the two
// therefore always leaves "file present, not yet indexed" -- a state Load
// already treats as ErrCorruptState ("unexpected") -- and can never leave
// "indexed, but the file was never written", which Load treats as a lost
// fence requiring reconciliation. Reserving that second, stronger-sounding
// case exclusively for genuine loss (a file that existed and was later
// deleted) rather than an ordinary startup crash is the reason for this
// ordering.
//
// Save performs no monotonicity check of its own; callers
// (internal/agent/client) must already have compared against Load's result
// before calling Save, per Decision 5's "never reset locally".
func (s Store) Save(fence Fence) error {
	if !fence.valid() {
		return errors.New("invalid fence state")
	}
	path, err := s.path(fence.EnvironmentID)
	if err != nil {
		return err
	}
	contents, err := json.Marshal(fence)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.dir, path, contents); err != nil {
		return err
	}
	return s.addToIndex(fence.EnvironmentID)
}

// loadIndex reads the durable manifest. failClosed=true means the caller
// must not trust any per-environment file in this store: either the
// manifest exists but cannot be parsed/validated, or it is missing while
// at least one fence file is present (a manifest that should exist but
// does not). A genuinely empty/missing manifest with no fence files at all
// is the legitimate brand-new-directory case and returns an empty, trusted
// set.
func (s Store) loadIndex() (listed map[string]bool, failClosed bool) {
	contents, err := os.ReadFile(filepath.Join(s.dir, indexFileName))
	if errors.Is(err, os.ErrNotExist) {
		if s.hasAnyFenceFile() {
			return nil, true
		}
		return map[string]bool{}, false
	}
	if err != nil {
		return nil, true
	}
	var idx index
	if jsonErr := json.Unmarshal(contents, &idx); jsonErr != nil {
		return nil, true
	}
	listed = make(map[string]bool, len(idx.Environments))
	for _, id := range idx.Environments {
		if !github.ValidOpaqueID(id) {
			return nil, true
		}
		listed[id] = true
	}
	return listed, false
}

// hasAnyFenceFile reports whether dir contains any *.json file other than
// the index manifest itself. A glob failure is treated as "yes" so a
// filesystem error never masquerades as a trustworthy brand-new directory.
func (s Store) hasAnyFenceFile() bool {
	matches, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return true
	}
	for _, m := range matches {
		if filepath.Base(m) != indexFileName {
			return true
		}
	}
	return false
}

// addToIndex durably adds environmentID to the manifest. It reads the
// current manifest best-effort: if the manifest cannot be trusted (see
// loadIndex), it rebuilds one containing only environmentID rather than
// propagate untrusted prior entries. This never weakens Load's guarantee --
// any environment thereby dropped from the manifest simply becomes a
// "file present, not indexed" ErrCorruptState the next time it is loaded,
// which is the same fail-closed outcome real corruption already requires.
func (s Store) addToIndex(environmentID string) error {
	listed, failClosed := s.loadIndex()
	if failClosed || listed == nil {
		listed = map[string]bool{}
	}
	if listed[environmentID] {
		return nil
	}
	listed[environmentID] = true
	ids := make([]string, 0, len(listed))
	for id := range listed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	contents, err := json.Marshal(index{Environments: ids})
	if err != nil {
		return err
	}
	return writeFileAtomic(s.dir, filepath.Join(s.dir, indexFileName), contents)
}

func writeFileAtomic(dir, path string, contents []byte) error {
	tmp, err := os.CreateTemp(dir, ".fence-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(contents); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	removeTemp = false
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
