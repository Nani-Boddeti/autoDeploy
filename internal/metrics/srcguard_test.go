package metrics

import (
	"testing"

	"autodeploy/internal/testutil/srcguard"
)

// The metrics package must never write to process output directly.
func TestNoProcessLoggingInSource(t *testing.T) {
	srcguard.CheckPackageDir(t, ".")
}
