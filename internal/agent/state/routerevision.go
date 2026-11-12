package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"autodeploy/internal/github"
)

// ErrRouteRevisionRegressed is returned when RecordRouteRevision is asked to
// store a revision lower than the one already recorded for the environment
// (owner decision D-C: the agent never writes an older route revision).
var ErrRouteRevisionRegressed = errors.New("route revision regressed")

// RouteRevisionStore persists, per environment, the highest route revision
// this agent has written. It is separate from Fence so Fence's file format is
// untouched.
// One agent process owns a state dir: exclusion between instances is
// in-process only; cross-process exclusion is not provided. The zero value is
// unusable and its methods return errors.
type RouteRevisionStore struct {
	dir string
}

// ErrInvalidRouteRevision reports an unusable environment id or revision.
var ErrInvalidRouteRevision = errors.New("invalid route revision input")

// maxFileNameBytes is the usual filesystem limit on one path component.
const maxFileNameBytes = 255

type routeRevisionFile struct {
	EnvironmentID string `json:"environment_id"`
	Revision      int64  `json:"revision"`
}

// RouteRevisions returns the route-revision store rooted at
// <stateDir>/route-revisions, creating the directory (0700) on first use.
func (s Store) RouteRevisions() (RouteRevisionStore, error) {
	if s.dir == "" {
		return RouteRevisionStore{}, errUninitialized
	}
	dir := filepath.Join(s.dir, routeRevisionsDirName)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return RouteRevisionStore{}, fmt.Errorf("create route revision directory: %w", err)
	}
	if err := syncDirAndParent(dir); err != nil {
		return RouteRevisionStore{}, fmt.Errorf("sync route revision directory: %w", err)
	}
	return RouteRevisionStore{dir: dir}, nil
}

func (r RouteRevisionStore) path(environmentID string) (string, error) {
	if r.dir == "" {
		return "", errUninitialized
	}
	if !github.ValidOpaqueID(environmentID) {
		return "", fmt.Errorf("%w: invalid environment id", ErrInvalidRouteRevision)
	}
	name, ok := fileNameFor(environmentID, routeRevisionFileSuffix)
	if !ok {
		return "", fmt.Errorf("%w: environment id too long to store", ErrInvalidRouteRevision)
	}
	return filepath.Join(r.dir, name), nil
}

func (r RouteRevisionStore) read(environmentID, path string) (int64, error) {
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, corrupt(err)
	}
	var f routeRevisionFile
	if err := decodeStrict(contents, &f); err != nil {
		return 0, corrupt(err)
	}
	if f.EnvironmentID != environmentID || f.Revision <= 0 {
		return 0, corrupt(errors.New("route revision file content invalid"))
	}
	return f.Revision, nil
}

// LoadRouteRevision returns the highest recorded revision, or 0 when none.
//
// Accepted gap: a missing file reads as 0 (fails open), so losing the file
// removes this guard. The guard is defence in depth over the control plane's
// compare-and-swap; losing it never authorizes a write the server did not.
// A present but untrustworthy file is ErrCorruptState.
func (r RouteRevisionStore) LoadRouteRevision(environmentID string) (int64, error) {
	path, err := r.path(environmentID)
	if err != nil {
		return 0, err
	}
	mu := lockDir(r.dir)
	mu.Lock()
	defer mu.Unlock()
	return r.read(environmentID, path)
}

// RecordRouteRevision durably records rev. A rev below the stored one returns
// ErrRouteRevisionRegressed and leaves the stored value unchanged; an equal
// rev is a no-op. A corrupt stored file is ErrCorruptState (fail closed).
func (r RouteRevisionStore) RecordRouteRevision(environmentID string, rev int64) error {
	if rev <= 0 {
		return fmt.Errorf("%w: revision must be positive", ErrInvalidRouteRevision)
	}
	path, err := r.path(environmentID)
	if err != nil {
		return err
	}
	mu := lockDir(r.dir)
	mu.Lock()
	defer mu.Unlock()
	cur, err := r.read(environmentID, path)
	if err != nil {
		return err
	}
	if rev < cur {
		return ErrRouteRevisionRegressed
	}
	if rev == cur {
		return nil
	}
	contents, err := json.Marshal(routeRevisionFile{EnvironmentID: environmentID, Revision: rev})
	if err != nil {
		return err
	}
	return writeFileAtomic(r.dir, path, contents)
}
