package postgres

import (
	"math"
	"testing"

	"autodeploy/internal/deployment"
)

func TestPostgresRevisionRejectsUnsignedOverflow(t *testing.T) {
	if _, err := postgresRevision(deployment.Revision(math.MaxInt64)); err != nil {
		t.Fatalf("MaxInt64: %v", err)
	}
	if _, err := postgresRevision(deployment.Revision(math.MaxInt64) + 1); err != ErrRevisionOverflow {
		t.Fatalf("overflow = %v", err)
	}
}
