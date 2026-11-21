//go:build unix

package gitcheckout

import (
	"errors"
	"testing"

	"autodeploy/internal/checkoutquota"
)

// A retryable release failure keeps the descriptor and a later Close retries;
// ErrDenied is terminal and still closes the descriptor.
func TestResultCloseRetriesAfterRetryableFailure(t *testing.T) {
	q := &fakeQuota{alloc: qaAllocation(t), releaseErrs: []error{checkoutquota.ErrRetryable, checkoutquota.ErrRetryable, nil}}
	r := &Result{quota: q, alloc: q.alloc}
	for i := 0; i < 2; i++ {
		if err := r.Close(); !errors.Is(err, checkoutquota.ErrRetryable) || r.WorkspaceFD() < 0 {
			t.Fatalf("attempt %d: err=%v fd=%d (fd must be retained)", i, err, r.WorkspaceFD())
		}
	}
	if err := r.Close(); err != nil || r.WorkspaceFD() != -1 {
		t.Fatalf("err=%v fd=%d", err, r.WorkspaceFD())
	}
	if len(q.calls) != 3 {
		t.Fatalf("calls=%v", q.calls)
	}
}

func TestResultCloseDeniedClosesDescriptor(t *testing.T) {
	q := &fakeQuota{alloc: qaAllocation(t), releaseErrs: []error{checkoutquota.ErrDenied}}
	r := &Result{quota: q, alloc: q.alloc}
	if err := r.Close(); !errors.Is(err, checkoutquota.ErrDenied) || r.WorkspaceFD() != -1 {
		t.Fatalf("err=%v fd=%d", err, r.WorkspaceFD())
	}
}
