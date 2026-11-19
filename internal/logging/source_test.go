package logging

import (
	"testing"

	"autodeploy/internal/testutil/srcguard"
)

func TestProductionSourceHasNoDirectProcessOutput(t *testing.T) {
	srcguard.CheckPackageDir(t, ".")
}
