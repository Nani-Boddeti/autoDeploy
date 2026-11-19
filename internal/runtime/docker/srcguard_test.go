package docker

import (
	"testing"

	"autodeploy/internal/testutil/srcguard"
)

// The agent journal must only be written through the injected, redacting
// *slog.Logger: no process stderr/stdout, stdlib log or slog default.
func TestNoProcessLoggingInSource(t *testing.T) {
	srcguard.CheckPackageDir(t, ".")
}
