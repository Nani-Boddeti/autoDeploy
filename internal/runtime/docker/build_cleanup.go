package docker

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"time"
)

const (
	// Bounds for the dedicated builder's cache (agent flag --build-cache-keep).
	MinBuildCacheKeepBytes     int64 = 1 << 30
	MaxBuildCacheKeepBytes     int64 = 500 << 30
	DefaultBuildCacheKeepBytes int64 = 5 << 30

	// DefaultPruneTimeout bounds one cache prune, so a wedged buildx can never
	// hold the agent after a build.
	DefaultPruneTimeout = 2 * time.Minute
)

// pruneTimeout is DefaultPruneTimeout, a variable only so tests can shorten it.
var pruneTimeout = DefaultPruneTimeout

// pruneBuildCache bounds the dedicated builder's build cache to keepBytes:
// BuildKit evicts the least recently used unused records until the cache is at
// or below the bound. It touches only the builder named BuilderName (explicit
// --builder; BUILDX_CONFIG pinned like every other buildx call), never another
// builder, and never an image: a --load build leaves its result as an untagged
// image that carries our labels and is exactly what the deploy runs, so image
// removal stays with the existing cleaners.
//
// --max-used-space is the flag that literally means "keep the cache under N"
// (BuildKit: keepBytes = max(max-used-space, reserved-space)). It is present in
// buildx v0.31.1 through v0.39.0 (source-verified; --keep-storage is only a
// deprecated alias of --reserved-space there) and needs BuildKit >= 0.17 (the pinned image is
// far newer). --force only skips the interactive confirmation.
func pruneBuildCache(ctx context.Context, cfg BuilderConfig, keepBytes int64) error {
	if cfg.ConfigDir == "" || !filepath.IsAbs(cfg.ConfigDir) {
		return fmt.Errorf("build cache prune: buildx config directory must be an absolute path")
	}
	if keepBytes < MinBuildCacheKeepBytes || keepBytes > MaxBuildCacheKeepBytes {
		return fmt.Errorf("build cache prune: keep %d outside %d..%d bytes", keepBytes, MinBuildCacheKeepBytes, MaxBuildCacheKeepBytes)
	}
	ctx, cancel := context.WithTimeout(ctx, pruneTimeout)
	defer cancel()
	_, err := runBuildx(ctx, cfg, "buildx", "prune", "--builder", BuilderName, "--max-used-space", strconv.FormatInt(keepBytes, 10), "--force")
	return err
}
