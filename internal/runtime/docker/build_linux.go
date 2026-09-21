//go:build linux

package docker

import "context"

// Build runs one `docker buildx build` invocation and returns the built
// image's immutable digests. See BuildRequest/BuildResult.
func Build(ctx context.Context, req BuildRequest) (BuildResult, error) {
	return build(ctx, req)
}

// ProbeBuildx is the buildx half of the startup capability probe: it fails
// closed when the docker CLI or the buildx plugin is missing or unusable,
// so an incapable host is caught at enrollment rather than mid-deployment.
func ProbeBuildx(ctx context.Context) error {
	return probeBuildx(ctx)
}
