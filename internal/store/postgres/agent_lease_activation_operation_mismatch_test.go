//go:build integration

package postgres

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"
)

// A report naming a different operation id than the attempt's activation
// operation answers ErrAgentLeaseOperationMismatch and records nothing. It must
// not be ErrAgentLeaseIntegrityConflict, which on this route tells the agent
// the outcome is terminal or the ambiguity recorded.
func TestReportActivationResultOperationMismatchRecordsNothing(t *testing.T) {
	a := newActivatedFixture(t, 120, true)
	before := a.resolveState(t)

	wrongID := a.operationID + "0"
	if n, err := strconv.ParseInt(a.operationID, 10, 64); err == nil {
		wrongID = strconv.FormatInt(n+1000000, 10)
	}
	err := a.repo.ReportActivationResult(context.Background(), a.serverID, a.cred, a.attemptID, 1, wrongID, true, a.routeDigest, "")
	if !errors.Is(err, ErrAgentLeaseOperationMismatch) {
		t.Fatalf("err = %v, want ErrAgentLeaseOperationMismatch", err)
	}
	if errors.Is(err, ErrAgentLeaseIntegrityConflict) {
		t.Fatalf("operation mismatch must not match ErrAgentLeaseIntegrityConflict: %v", err)
	}
	if after := a.resolveState(t); !reflect.DeepEqual(before, after) {
		t.Fatalf("state changed by a mismatched report:\nbefore %+v\nafter  %+v", before, after)
	}
}
