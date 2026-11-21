//go:build !linux

// checkout-exec is Linux-only (cgroup v2, XFS project quotas, setuid
// helpers). This stub keeps the package buildable elsewhere so its
// platform-independent pieces (classify.go) build and test on any OS.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "checkout-exec: supported on linux only")
	os.Exit(1)
}
