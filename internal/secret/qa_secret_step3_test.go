package secret

import (
	"context"
	"testing"
)

// This test exercises the operator-facing restart contract: repeatedly feed the
// emitted cursor into the next bounded batch, then require that no old-key
// references remain. A transient CAS conflict must not be paginated away.
func TestQAStep3CursorResumeRetriesCASConflictsBeforeRetirement(t *testing.T) {
	provider := rotationProvider{
		active: "new",
		keys: map[string][]byte{
			"old": []byte("01234567890123456789012345678901"),
			"new": []byte("abcdefghijklmnopqrstuvwxyz123456"),
		},
	}
	repository := &rotationRepo{}
	repository.versions = []Version{
		rotationVersionWith(t, "a", "old", provider.keys["old"]),
		rotationVersionWith(t, "b", "old", provider.keys["old"]),
	}
	repository.conflict = "a"
	rotator := NewRotator(repository, provider)

	first, err := rotator.RewrapBatch(context.Background(), RewrapRequest{
		OldKEKID:  "old",
		BatchSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Conflicts != 1 {
		t.Fatalf("conflicts = %d, want 1", first.Conflicts)
	}

	// The failed row remains on the old KEK. Resume exactly from the last
	// successful cursor so it is selected and retried before later rows.
	repository.conflict = ""
	second, err := rotator.RewrapBatch(context.Background(), RewrapRequest{
		OldKEKID:  "old",
		Cursor:    first.NextCursor,
		BatchSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Selected != 2 || second.Rewrapped != 2 {
		t.Fatalf("second result = %+v, want both rows retried", second)
	}

	remaining := 0
	for _, version := range repository.versions {
		if version.Envelope.KEKID == "old" {
			remaining++
		}
	}
	if remaining != 0 {
		t.Fatalf("cursor-resumed rotation left %d old-KEK reference(s)", remaining)
	}
}
