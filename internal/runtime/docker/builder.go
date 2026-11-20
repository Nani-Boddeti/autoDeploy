package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"autodeploy/internal/logging"
)

const (
	// BuilderName is the one buildx builder every build runs in. The agent
	// only ever creates, inspects or removes a builder with this exact name.
	BuilderName = "autodeploy-build"

	// BuildkitImage is the BuildKit daemon image the docker-container driver
	// runs, pinned by the multi-arch index digest (moby/buildkit v0.35.0, the
	// latest stable release at the time of writing; digest read with
	// `docker buildx imagetools inspect`). Bump tag and digest together.
	BuildkitImage = "moby/buildkit:v0.35.0@sha256:aa632b921ef07411d2cb77292cd34213d4f7b384001d6fceef1e1f25bf9752bb"

	// Bounds for the resource caps (agent flags --build-memory/--build-cpus).
	MinBuildMemoryBytes int64 = 512 << 20
	MaxBuildMemoryBytes int64 = 32 << 30
	DefaultBuildMemory  int64 = 2 << 30
	MinBuildCPUs              = 1
	MaxBuildCPUs              = 32
	DefaultBuildCPUs          = 2

	// cpuPeriodMicros is the CFS period; the quota is CPUs * period.
	cpuPeriodMicros = 100000

	// builderSetupTimeout bounds one whole ensure run (it may pull the
	// BuildKit image).
	builderSetupTimeout = 5 * time.Minute
)

// ErrBuilderUnavailable means the dedicated capped builder could not be
// created, matched or bootstrapped. The agent fails closed: it never builds
// in the default (uncapped) builder instead.
var ErrBuilderUnavailable = errors.New("docker buildx: dedicated builder unavailable")

// BuilderConfig is the dedicated builder's resource caps and state location.
type BuilderConfig struct {
	// ConfigDir is the BUILDX_CONFIG directory: where buildx keeps the
	// builder definition. It must be absolute, writable and agent-owned.
	ConfigDir   string
	MemoryBytes int64
	CPUs        int
	// Logger receives one agent-journal record whenever the builder is
	// recreated (and why). Nil discards; there is no fallback to a
	// package-level logger. Never given secrets.
	Logger *slog.Logger
}

func (c BuilderConfig) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (c BuilderConfig) validate() error {
	switch {
	case c.ConfigDir == "" || !filepath.IsAbs(c.ConfigDir):
		return fmt.Errorf("%w: buildx config directory must be an absolute path", ErrBuilderUnavailable)
	case c.MemoryBytes < MinBuildMemoryBytes || c.MemoryBytes > MaxBuildMemoryBytes:
		return fmt.Errorf("%w: build memory %d outside %d..%d bytes", ErrBuilderUnavailable, c.MemoryBytes, MinBuildMemoryBytes, MaxBuildMemoryBytes)
	case c.CPUs < MinBuildCPUs || c.CPUs > MaxBuildCPUs:
		return fmt.Errorf("%w: build cpus %d outside %d..%d", ErrBuilderUnavailable, c.CPUs, MinBuildCPUs, MaxBuildCPUs)
	}
	return nil
}

// driverOpts is the exact driver-opt set of the builder. memory-swap equals
// memory so the cap is hard (Docker otherwise allows swap up to twice the
// memory limit).
func (c BuilderConfig) driverOpts() map[string]string {
	mem := strconv.FormatInt(c.MemoryBytes, 10)
	return map[string]string{
		"image":       BuildkitImage,
		"memory":      mem,
		"memory-swap": mem,
		"cpu-period":  strconv.Itoa(cpuPeriodMicros),
		"cpu-quota":   strconv.Itoa(c.CPUs * cpuPeriodMicros),
	}
}

// driverOptArg renders driverOpts in a fixed order for --driver-opt.
func (c BuilderConfig) driverOptArg() string {
	o := c.driverOpts()
	parts := make([]string, 0, len(o))
	for _, k := range []string{"image", "memory", "memory-swap", "cpu-period", "cpu-quota"} {
		parts = append(parts, k+"="+o[k])
	}
	return strings.Join(parts, ",")
}

// withBuildxConfig returns env with BUILDX_CONFIG pinned to dir and any
// ambient BUILDX_BUILDER dropped (the builder is always named by --builder).
// An empty dir leaves env untouched.
func withBuildxConfig(env []string, dir string) []string {
	if dir == "" {
		return env
	}
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if strings.HasPrefix(e, "BUILDX_CONFIG=") || strings.HasPrefix(e, "BUILDX_BUILDER=") {
			continue
		}
		out = append(out, e)
	}
	return append(out, "BUILDX_CONFIG="+dir)
}

// maxBuildxOutputBytes caps how much of a buildx subcommand's stdout or
// stderr is held in memory (F12f). Legitimate `buildx ls/inspect` output is a
// few KiB; anything beyond the cap is discarded, not an error.
const maxBuildxOutputBytes = 1 << 20

// cappedBuffer keeps at most max bytes and silently discards the rest while
// still reporting full writes, so the child never sees a short write or EPIPE.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		c.buf.Write(p[:room])
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }

// runBuildx runs `docker <args>` in the builder's config dir and returns its
// stdout; stderr is folded into the error text.
func runBuildx(ctx context.Context, cfg BuilderConfig, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, dockerExecutable, args...)
	killProcessGroupOnCancel(cmd)
	cmd.WaitDelay = buildWaitDelay
	cmd.Env = withBuildxConfig(subprocessEnv(nil), cfg.ConfigDir)
	stdout := &cappedBuffer{max: maxBuildxOutputBytes}
	stderr := &cappedBuffer{max: maxBuildxOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 512 {
			msg = msg[:512]
		}
		return "", fmt.Errorf("%w: docker %s: %v: %s", ErrBuilderUnavailable, strings.Join(args[:2], " "), err, msg)
	}
	return stdout.String(), nil
}

type listedBuilder struct {
	Name   string `json:"Name"`
	Driver string `json:"Driver"`
	Nodes  []struct {
		DriverOpts map[string]string `json:"DriverOpts"`
	} `json:"Nodes"`
}

// builderState reports whether the named builder exists and whether it
// already has the wanted driver and exactly the wanted driver options. When it
// exists but does not match, reason says why (driver, or which driver-opt key
// differs or is absent; values are never included, they can carry the image
// reference and are not needed to act).
func builderState(listing string, want map[string]string) (exists, matches bool, reason string, err error) {
	dec := json.NewDecoder(strings.NewReader(listing))
	for {
		var b listedBuilder
		if err := dec.Decode(&b); err != nil {
			if errors.Is(err, io.EOF) {
				return false, false, "", nil
			}
			return false, false, "", fmt.Errorf("%w: unparsable builder list: %v", ErrBuilderUnavailable, err)
		}
		if b.Name != BuilderName {
			continue
		}
		if b.Driver != "docker-container" {
			return true, false, fmt.Sprintf("driver %q is not docker-container", b.Driver), nil
		}
		if len(b.Nodes) != 1 {
			return true, false, fmt.Sprintf("%d nodes, want 1", len(b.Nodes)), nil
		}
		have := b.Nodes[0].DriverOpts
		if len(have) == 0 {
			// Some buildx versions may not print DriverOpts in `ls`. The caps
			// cannot be verified, so the builder is recreated (the previous
			// behaviour); this runs at startup and after a failed build only,
			// never per build, so it is not a hot loop.
			return true, false, "`buildx ls` reports no driver options, so the caps cannot be verified", nil
		}
		for _, k := range []string{"image", "memory", "memory-swap", "cpu-period", "cpu-quota"} {
			v, ok := have[k]
			switch {
			case !ok:
				return true, false, "driver-opt " + k + " absent", nil
			case v != want[k]:
				return true, false, "driver-opt " + k + " differs", nil
			}
		}
		if len(have) != len(want) {
			return true, false, "unexpected extra driver options", nil
		}
		return true, true, "", nil
	}
}

// ensureBuilder idempotently makes the dedicated builder exist with the
// configured caps and be bootstrapped. A builder that exists with a different
// driver, image or caps (a flag changed, or it was edited) is removed and
// recreated: it holds only rebuildable cache and its own container, and a
// stale cap would silently ignore the new flag. Builders with any other name
// are never touched. Everything fails closed with ErrBuilderUnavailable.
func ensureBuilder(ctx context.Context, cfg BuilderConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		return fmt.Errorf("%w: create config dir: %v", ErrBuilderUnavailable, err)
	}
	// MkdirAll leaves an existing directory's mode alone; the builder
	// definition must stay agent-private whatever it was created with.
	if fi, err := os.Stat(cfg.ConfigDir); err != nil {
		return fmt.Errorf("%w: stat config dir: %v", ErrBuilderUnavailable, err)
	} else if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(cfg.ConfigDir, 0o700); err != nil {
			return fmt.Errorf("%w: restrict config dir: %v", ErrBuilderUnavailable, err)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, builderSetupTimeout)
	defer cancel()

	listing, err := runBuildx(ctx, cfg, "buildx", "ls", "--format", "json")
	if err != nil {
		return err
	}
	exists, matches, reason, err := builderState(listing, cfg.driverOpts())
	if err != nil {
		return err
	}
	if !matches {
		if exists {
			cfg.logger().Info("buildx builder recreating", logging.KeyComponent, "builder", "builder", BuilderName, "reason", reason)
			if _, err := runBuildx(ctx, cfg, "buildx", "rm", BuilderName); err != nil {
				return err
			}
		}
		if _, err := runBuildx(ctx, cfg, "buildx", "create", "--name", BuilderName, "--driver", "docker-container", "--driver-opt", cfg.driverOptArg()); err != nil {
			return err
		}
	}
	// Bootstrap proves the builder container can actually start (and starts
	// it again after a host reboot).
	out, err := runBuildx(ctx, cfg, "buildx", "inspect", "--builder", BuilderName, "--bootstrap")
	if err != nil {
		return err
	}
	if !strings.Contains(out, "docker-container") {
		return fmt.Errorf("%w: unexpected inspect output", ErrBuilderUnavailable)
	}
	return nil
}

// builderCheckTimeout bounds the cheap post-failure builder inspection.
const builderCheckTimeout = 30 * time.Second

// checkBuilderAfterFailure runs after a build subprocess exited non-zero and
// tells an infrastructure loss (builder removed, its state dir wiped, its
// container deleted) apart from a genuine Dockerfile failure. It returns nil
// when the builder is present and usable (the failure is the tenant's), and
// otherwise an error wrapping ErrBuilderUnavailable after one attempt to
// recreate the builder via ensureBuilder; the failed build itself is never
// retried in-process. BuilderConfig that cannot drive ensureBuilder (no caps
// supplied) skips only the recreation, not the verdict.
func checkBuilderAfterFailure(ctx context.Context, cfg BuilderConfig) error {
	inspectCtx, cancel := context.WithTimeout(ctx, builderCheckTimeout)
	out, err := runBuildx(inspectCtx, cfg, "buildx", "inspect", "--builder", BuilderName)
	cancel()
	if err == nil && strings.Contains(out, "docker-container") {
		return nil
	}
	if ctx.Err() != nil {
		return nil // the caller's own cancellation is classified by the caller
	}
	cause := "builder missing or unusable"
	if err != nil {
		cause = err.Error()
	}
	cfg.logger().Info("buildx builder missing or unusable after a failed build; recreating", logging.KeyComponent, "builder", "builder", BuilderName, "cause", cause)
	if cfg.validate() != nil {
		return fmt.Errorf("%w: missing or unusable after a failed build", ErrBuilderUnavailable)
	}
	if rerr := ensureBuilder(ctx, cfg); rerr != nil {
		return fmt.Errorf("%w: missing or unusable after a failed build; recreation failed: %v", ErrBuilderUnavailable, rerr)
	}
	return fmt.Errorf("%w: missing after a failed build and recreated; retry the build", ErrBuilderUnavailable)
}
