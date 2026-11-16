//go:build unix

package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"autodeploy/internal/queue"
)

// slowBuildDocker is a fake docker whose buildx build records its own pid
// and a child's pid, ignores SIGTERM, and then sleeps far past any test
// limit, so only a group SIGKILL can end it.
func slowBuildDocker(t *testing.T, pidFile string) {
	t.Helper()
	writeFakeDocker(t, `
if [ "$1" = "buildx" ] && [ "$2" = "version" ]; then
  echo "github.com/docker/buildx v0.17.0"
  exit 0
fi
if [ "$1" = "buildx" ] && [ "$2" = "build" ]; then
  trap '' TERM
  sleep 300 &
  echo "$$ $!" > "`+pidFile+`"
  sleep 300
  exit 0
fi
exit 1
`)
}

// readPIDs waits for the fake docker's pid file. It returns an error instead
// of failing the test so it is safe to call from a spawned goroutine.
func readPIDs(pidFile string) ([]int, error) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			if f := strings.Fields(string(b)); len(f) == 2 {
				var pids []int
				for _, s := range f {
					n, err := strconv.Atoi(s)
					if err != nil {
						return nil, fmt.Errorf("pid file: %w", err)
					}
					pids = append(pids, n)
				}
				return pids, nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, errors.New("fake docker never wrote its pids")
}

func mustReadPIDs(t *testing.T, pidFile string) []int {
	t.Helper()
	pids, err := readPIDs(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	return pids
}

func requireGone(t *testing.T, pids []int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for _, pid := range pids {
		for syscall.Kill(pid, 0) == nil {
			if time.Now().After(deadline) {
				t.Fatalf("process %d still alive after the build returned", pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func TestBuildTimeoutKillsProcessGroupAndClassifiesPermanent(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	slowBuildDocker(t, pidFile)
	req := testBuildRequest(t, t.TempDir())
	// Generous: the kill must come from the limit, but the fake needs time
	// to start and write its pid file under -race load.
	req.Timeout = 2 * time.Second

	start := time.Now()
	_, err := build(context.Background(), req)
	if !errors.Is(err, ErrBuildTimeout) {
		t.Fatalf("want ErrBuildTimeout, got %v", err)
	}
	if errors.Is(err, ErrBuildFailed) || errors.Is(err, ErrBuildInterrupted) {
		t.Fatalf("timeout must be its own class of error, got %v", err)
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Fatalf("build took %s; a stuck child must not hang Wait", took)
	}
	if got := ClassifyFailure(err); got != queue.FailureClassPermanentConfiguration {
		t.Fatalf("class = %s, want permanent_configuration", got)
	}
	requireGone(t, mustReadPIDs(t, pidFile))
}

func TestBuildParentCancelIsTransientNotPermanent(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	slowBuildDocker(t, pidFile)
	req := testBuildRequest(t, t.TempDir())
	req.Timeout = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	pidErr := make(chan error, 1)
	go func() {
		_, err := readPIDs(pidFile)
		pidErr <- err
		cancel()
	}()
	_, err := build(ctx, req)
	if perr := <-pidErr; perr != nil {
		t.Fatal(perr)
	}
	if !errors.Is(err, ErrBuildInterrupted) || !errors.Is(err, context.Canceled) {
		t.Fatalf("want ErrBuildInterrupted wrapping context.Canceled, got %v", err)
	}
	if errors.Is(err, ErrBuildTimeout) || errors.Is(err, ErrBuildFailed) {
		t.Fatalf("a parent cancel must not look like a timeout or a failed build: %v", err)
	}
	if got := ClassifyFailure(err); got != queue.FailureClassTransient {
		t.Fatalf("class = %s, want transient", got)
	}
	requireGone(t, mustReadPIDs(t, pidFile))
}

func TestBuildParentDeadlineIsTransientNotTimeout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	slowBuildDocker(t, pidFile)
	req := testBuildRequest(t, t.TempDir())
	req.Timeout = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := build(ctx, req)
	if !errors.Is(err, ErrBuildInterrupted) || errors.Is(err, ErrBuildTimeout) {
		t.Fatalf("a parent deadline is a lease problem, not our limit: %v", err)
	}
	if got := ClassifyFailure(err); got != queue.FailureClassTransient {
		t.Fatalf("class = %s, want transient", got)
	}
}

func TestBuildNonZeroExitStaysPermanentBuildFailed(t *testing.T) {
	writeFakeDocker(t, `
if [ "$1" = "buildx" ] && [ "$2" = "version" ]; then
  echo "github.com/docker/buildx v0.17.0"
  exit 0
fi
if [ "$1" = "buildx" ] && [ "$2" = "inspect" ]; then
  echo "Driver: docker-container"
  exit 0
fi
echo "boom" >&2
exit 3
`)
	req := testBuildRequest(t, t.TempDir())
	req.Timeout = time.Minute
	_, err := build(context.Background(), req)
	if !errors.Is(err, ErrBuildFailed) || errors.Is(err, ErrBuildTimeout) || errors.Is(err, ErrBuildInterrupted) {
		t.Fatalf("want plain ErrBuildFailed, got %v", err)
	}
	if got := ClassifyFailure(err); got != queue.FailureClassPermanentConfiguration {
		t.Fatalf("class = %s, want permanent_configuration", got)
	}
}

func TestBuildSucceedsWithinTimeout(t *testing.T) {
	imageDigest := "sha256:" + strings.Repeat("1", 64)
	writeFakeDocker(t, `
if [ "$1" = "buildx" ] && [ "$2" = "version" ]; then
  echo "github.com/docker/buildx v0.17.0"
  exit 0
fi
meta=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--metadata-file" ]; then meta="$a"; fi
  prev="$a"
done
printf '{"containerimage.digest":"`+imageDigest+`"}' > "$meta"
exit 0
`)
	req := testBuildRequest(t, t.TempDir())
	req.Timeout = time.Minute
	res, err := build(context.Background(), req)
	if err != nil || res.ImageDigest != imageDigest {
		t.Fatalf("build = %+v, %v", res, err)
	}
}

func TestEffectiveBuildTimeoutDefaults(t *testing.T) {
	if got := EffectiveBuildTimeout(0); got != DefaultBuildTimeout {
		t.Fatalf("zero -> %s, want default %s", got, DefaultBuildTimeout)
	}
	if got := EffectiveBuildTimeout(5 * time.Minute); got != 5*time.Minute {
		t.Fatalf("got %s", got)
	}
}

func TestBuildParentCancelDuringProbeIsInterruptedNotUnavailable(t *testing.T) {
	writeFakeDocker(t, `
sleep 300
`)
	req := testBuildRequest(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := build(ctx, req)
	if !errors.Is(err, ErrBuildInterrupted) || errors.Is(err, ErrBuildxUnavailable) {
		t.Fatalf("a cancelled probe must read as interrupted: %v", err)
	}
	if got := ClassifyFailure(err); got != queue.FailureClassTransient {
		t.Fatalf("class = %s, want transient", got)
	}
}
