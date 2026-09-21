//go:build !linux

package docker

import "context"

// Build is unavailable outside Linux, mirroring NewClient: the agent's
// Docker/buildx surface targets Linux hosts only, and callers must fail
// closed rather than receive a builder that could silently no-op.
func Build(context.Context, BuildRequest) (BuildResult, error) {
	return BuildResult{}, ErrUnsupportedPlatform
}

// ProbeBuildx is unavailable outside Linux; see Build.
func ProbeBuildx(context.Context) error {
	return ErrUnsupportedPlatform
}
