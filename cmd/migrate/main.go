// Command migrate applies AutoDeploy's embedded forward-only PostgreSQL migrations.
package main

import (
	"context"
	"os"
)

// main is the composition root, like cmd/controlplane: it alone hands the
// process stderr to run, which writes only fixed, secret-free messages (see
// run.go; srcguard_test.go forbids any other process output in this package).
func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stderr))
}
