//go:build linux

package executor

import (
	"fmt"
	"os"
)

// contextDirFromWorkspace resolves the checkout workspace descriptor to a
// filesystem path buildx can use as a build context directory
// (internal/runtime/docker.BuildRequest.ContextDir wants a path, not a
// descriptor; internal/gitcheckout deliberately hands back only a
// descriptor -- see gitcheckout.Result.WorkspaceFD's doc comment).
//
// /proc/self/fd/<fd> is a magic symlink the kernel resolves to the exact
// open file description, not by walking pathname components from "/" --
// "self" here means this process, which still holds fd open (fork
// duplicates the fd table verbatim, so this also works from a forked child
// before exec, which is what os/exec's Dir-based chdir needs). Reading it
// once with os.Readlink gives a real, walkable pathname for
// build.go's own Dockerfile-path validation, which explicitly rejects the
// context directory itself being a symlink (as every /proc/self/fd entry
// always is) -- so the raw magic-symlink path cannot be passed through
// unresolved.
//
// Residual risk, accepted on the same terms internal/runtime/docker/build.go
// already accepts for its own Dockerfile-path resolution: between this
// Readlink and buildx's own subprocess opening files under the resolved
// path, nothing prevents a rename of an intermediate component if another
// process has write access to the checkout root. The workspace descriptor
// stays open (via the *gitcheckout.Result the caller keeps alive) for the
// whole build, which prevents the checkout root itself from being torn
// down, but does not fully close a rename-based TOCTOU. Checkout and build
// run sequentially, never concurrently, within one Execute call, and the
// checkout root is root-owned/helper-created (internal/checkoutquota) with
// no other writer expected -- matching build.go's own "the window is small,
// revisit if checkout ever runs concurrently with the build" note.
func contextDirFromWorkspace(fd int) (string, error) {
	if fd < 0 {
		return "", fmt.Errorf("checkout workspace descriptor is unusable (fd=%d)", fd)
	}
	link := fmt.Sprintf("/proc/self/fd/%d", fd)
	real, err := os.Readlink(link)
	if err != nil {
		return "", fmt.Errorf("resolve checkout workspace path: %w", err)
	}
	if real == "" || real[0] != '/' {
		return "", fmt.Errorf("checkout workspace resolved to an unusable path %q", real)
	}
	return real, nil
}
