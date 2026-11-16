//go:build unix

package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPruneBuildCacheArgvIsExactlyScopedAndNonInteractive(t *testing.T) {
	logPath := fakeBuilderDocker(t, "", "")
	cfg := testBuilderConfig(t)
	if err := pruneBuildCache(context.Background(), cfg, 5<<30); err != nil {
		t.Fatalf("pruneBuildCache: %v", err)
	}
	calls := readCalls(t, logPath)
	want := cfg.ConfigDir + "|buildx prune --builder autodeploy-build --max-used-space 5368709120 --force"
	if len(calls) != 1 || calls[0] != want {
		t.Fatalf("calls = %q, want exactly [%q]", calls, want)
	}
}

func TestPruneBuildCacheRejectsOutOfBoundsKeepWithoutRunningDocker(t *testing.T) {
	for name, keep := range map[string]int64{
		"zero": 0, "below min": MinBuildCacheKeepBytes - 1, "above max": MaxBuildCacheKeepBytes + 1, "negative": -1,
	} {
		logPath := fakeBuilderDocker(t, "", "")
		if err := pruneBuildCache(context.Background(), testBuilderConfig(t), keep); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if _, err := os.Stat(logPath); err == nil {
			t.Errorf("%s: docker must not run for an invalid bound", name)
		}
	}
	logPath := fakeBuilderDocker(t, "", "")
	if err := pruneBuildCache(context.Background(), BuilderConfig{ConfigDir: "relative"}, 5<<30); err == nil {
		t.Error("a relative config dir must be rejected")
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Error("docker must not run for a relative config dir")
	}
}

func TestPruneBuildCacheReportsDockerFailure(t *testing.T) {
	fakeBuilderDocker(t, "", "prune")
	if err := pruneBuildCache(context.Background(), testBuilderConfig(t), 5<<30); err == nil || !strings.Contains(err.Error(), "prune") {
		t.Fatalf("err = %v, want one naming the prune failure", err)
	}
}

func TestPruneBuildCacheIsBoundedByItsOwnTimeout(t *testing.T) {
	if DefaultPruneTimeout != 2*time.Minute {
		t.Fatalf("DefaultPruneTimeout = %s, want 2m", DefaultPruneTimeout)
	}
	pidFile := filepath.Join(t.TempDir(), "pids")
	writeFakeDocker(t, `
if [ "$1" = "buildx" ] && [ "$2" = "prune" ]; then
  trap '' TERM
  sleep 300 &
  echo "$$ $!" > "`+pidFile+`"
  sleep 300
fi
exit 0
`)
	orig := pruneTimeout
	pruneTimeout = 2 * time.Second
	defer func() { pruneTimeout = orig }()

	start := time.Now()
	err := pruneBuildCache(context.Background(), testBuilderConfig(t), 5<<30)
	if err == nil {
		t.Fatal("a hung prune must return an error")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("prune took %s, want it bounded near its timeout", d)
	}
	requireGone(t, mustReadPIDs(t, pidFile))
}
