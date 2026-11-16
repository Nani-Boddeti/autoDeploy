//go:build linux

package docker

import "context"

// Build runs one `docker buildx build` invocation and returns the built
// image's immutable digests. See BuildRequest/BuildResult.
func Build(ctx context.Context, req BuildRequest) (BuildResult, error) {
	return build(ctx, req)
}

// ProbeBuildx is the buildx half of the startup capability probe: it fails
// closed when the docker CLI or the buildx plugin is missing or unusable, or
// when the dedicated capped builder cannot be ensured and bootstrapped, so an
// incapable host is caught at startup rather than mid-deployment.
func ProbeBuildx(ctx context.Context, cfg BuilderConfig) error {
	if err := probeBuildx(ctx); err != nil {
		return err
	}
	return ensureBuilder(ctx, cfg)
}

// PruneBuildCache bounds the dedicated builder's cache to keepBytes after a
// build; see pruneBuildCache. Errors are for logging: they never decide a
// deployment's outcome.
func PruneBuildCache(ctx context.Context, cfg BuilderConfig, keepBytes int64) error {
	return pruneBuildCache(ctx, cfg, keepBytes)
}
