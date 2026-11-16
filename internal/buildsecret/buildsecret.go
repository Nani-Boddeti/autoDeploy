// Package buildsecret is the single, pure definition of the opaque BuildKit
// session secret id a Dockerfile author uses in `RUN --mount=type=secret,id=...`.
//
// It is a leaf package (standard library only) so the agent's build path and
// the web config page can share it without the web layer importing
// internal/runtime/docker.
//
// Rule (ADR 0003 keeps the id opaque, never the variable name): the id of a
// variable is "build-secret-N", where N is the index of its name among the
// environment's build-scoped secret binding names sorted in byte order.
// Adding or removing a build secret binding can therefore shift the ids of
// names that sort after it.
package buildsecret

import (
	"slices"
	"strconv"
)

const prefix = "build-secret-"

// ID returns the id for name within names, the full set of build-scoped
// secret binding names (any order; duplicates are not expected). ok is false
// when name is not in names.
func ID(names []string, name string) (id string, ok bool) {
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	i, found := slices.BinarySearch(sorted, name)
	if !found {
		return "", false
	}
	return prefix + strconv.Itoa(i), true
}
