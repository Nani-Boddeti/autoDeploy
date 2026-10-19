package postgres

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAppendLateEvidenceSanitizesAndBounds(t *testing.T) {
	fp := lateReportFingerprint(false, [32]byte{}, "x")
	marker := lateReportMarker(fp)

	t.Run("control characters never reach the column", func(t *testing.T) {
		got, outcome := appendLateEvidence("base", fp, "a\x00b\nc\x7fd\u0085e\x1b", false)
		if outcome != lateEvidenceAppended {
			t.Fatalf("outcome=%v", outcome)
		}
		if strings.ContainsAny(got, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f\x7f") || strings.ContainsRune(got, '\u0085') {
			t.Fatalf("control character survived: %q", got)
		}
		if want := "base | " + marker + " a b c d e"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("never exceeds 2048 bytes and never splits a rune", func(t *testing.T) {
		// existing leaves room for the marker plus exactly 4 more bytes: with
		// 2-byte runes after the one-byte space the cut falls MID-rune and must
		// back off one byte.
		existing := strings.Repeat("a", maxOutcomeSummaryBytes-len(lateReportSeparator)-len(marker)-4)
		got, outcome := appendLateEvidence(existing, fp, strings.Repeat("é", 100), true)
		if outcome != lateEvidenceAppended {
			t.Fatalf("outcome=%v", outcome)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("invalid UTF-8 after truncation")
		}
		// room for the entry is len(marker)+4: marker, a space, then ONE whole
		// 2-byte rune (3 bytes of the 4); the second rune's first byte is the
		// 4th and must be dropped, leaving the result 1 byte under the cap.
		if len(got) != maxOutcomeSummaryBytes-1 || !strings.HasSuffix(got, marker+" é") {
			t.Fatalf("len=%d suffix=%q, want %d bytes ending %q", len(got), got[len(got)-10:], maxOutcomeSummaryBytes-1, marker+" é")
		}
	})

	t.Run("the marker is never cut: no room means nothing is written", func(t *testing.T) {
		existing := strings.Repeat("a", maxOutcomeSummaryBytes-len(lateReportSeparator)-len(marker)+1)
		got, outcome := appendLateEvidence(existing, fp, "evidence", true)
		if outcome != lateEvidenceNoRoom || got != existing {
			t.Fatalf("outcome=%v changed=%v, want no room and unchanged", outcome, got != existing)
		}
		// Exactly enough room for the marker alone still records it whole.
		existing = strings.Repeat("a", maxOutcomeSummaryBytes-len(lateReportSeparator)-len(marker))
		got, outcome = appendLateEvidence(existing, fp, "evidence", true)
		if outcome != lateEvidenceAppended || len(got) != maxOutcomeSummaryBytes || !strings.HasSuffix(got, marker) {
			t.Fatalf("outcome=%v len=%d, want the whole marker at the cap", outcome, len(got))
		}
	})

	t.Run("a fingerprint already present is a replay and changes nothing", func(t *testing.T) {
		once, _ := appendLateEvidence("base", fp, "evidence", false)
		again, outcome := appendLateEvidence(once, fp, "evidence", false)
		if outcome != lateEvidenceReplay || again != once {
			t.Fatalf("outcome=%v changed=%v", outcome, again != once)
		}
	})
}

func TestLateReportFingerprintDistinguishesReports(t *testing.T) {
	var d1, d2 [32]byte
	d2[0] = 1
	seen := map[string]string{}
	for name, fp := range map[string]string{
		"success d1":      lateReportFingerprint(true, d1, ""),
		"success d2":      lateReportFingerprint(true, d2, ""),
		"failure a":       lateReportFingerprint(false, [32]byte{}, "a"),
		"failure b":       lateReportFingerprint(false, [32]byte{}, "b"),
		"failure ignores": lateReportFingerprint(false, d2, "a"), // digest is not part of a failure report
	} {
		if len(fp) != 16 {
			t.Fatalf("%s: fingerprint %q is not 16 hex characters", name, fp)
		}
		if prev, dup := seen[fp]; dup && !(name == "failure ignores" && prev == "failure a") && !(name == "failure a" && prev == "failure ignores") {
			t.Fatalf("%s collides with %s", name, prev)
		}
		seen[fp] = name
	}
}

func TestIsOriginalBlockReplay(t *testing.T) {
	for name, tc := range map[string]struct {
		existing, summary string
		want              bool
	}{
		"exact":                         {"reload failed", "reload failed", true},
		"followed by late evidence":     {"reload failed | late report #0123456789abcdef: x", "reload failed", true},
		"a different report":            {"reload failed", "other", false},
		"summary containing a bar only": {"a | b", "a", false},
		"prefix without the separator":  {"reload failed twice", "reload failed", false},
	} {
		if got := isOriginalBlockReplay(tc.existing, tc.summary); got != tc.want {
			t.Fatalf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

// longestSuccessEntry is the longest success entry the real builders can
// produce ("false" is longer than "true"; the digest is always 64 hex).
func longestSuccessEntry(d [32]byte) string {
	fp := lateReportFingerprint(true, d, "")
	return lateReportMarker(fp) + " " + sanitizeBoundedText(lateSuccessBody(d, false), maxOutcomeSummaryBytes)
}

func TestLateSuccessEntryFitsTheReserve(t *testing.T) {
	entry := longestSuccessEntry([32]byte{0xff})
	// Two distinct entries: the first separator is already out of the room
	// appendLateEvidence computes, the second is not.
	if need := 2*len(entry) + len(lateReportSeparator); need > lateSuccessReserve {
		t.Fatalf("two longest success entries need %d bytes, reserve is %d", need, lateSuccessReserve)
	}
	// With the largest original summary, a full-length late failure entry
	// still fits beside the reserve.
	failureEntry := lateReportMarker(lateReportFingerprint(false, [32]byte{}, "")) + " " + lateFailureBody(strings.Repeat("x", maxActivationFailureSummaryBytes))
	if need := maxActivationFailureSummaryBytes + len(lateReportSeparator) + len(failureEntry) + lateSuccessReserve; need > maxOutcomeSummaryBytes {
		t.Fatalf("max original + one full failure entry + reserve = %d, over the %d cap", need, maxOutcomeSummaryBytes)
	}
}

func TestLateSuccessFlagPrecedesTheDigest(t *testing.T) {
	body := lateSuccessBody([32]byte{1}, false)
	if strings.Index(body, "matches authorized digest: false") > strings.Index(body, "sha256:") {
		t.Fatalf("flag must come before the digest so a cut cannot drop it: %q", body)
	}
}

func TestLateFailureEntriesLeaveTheSuccessReserve(t *testing.T) {
	existing := strings.Repeat("o", maxActivationFailureSummaryBytes)
	body := strings.Repeat("x", maxActivationFailureSummaryBytes)
	for i := 0; ; i++ {
		fp := lateReportFingerprint(false, [32]byte{}, strconv.Itoa(i))
		got, outcome := appendLateEvidence(existing, fp, body, false)
		if outcome == lateEvidenceNoRoom {
			break
		}
		if outcome != lateEvidenceAppended || len(got) > maxOutcomeSummaryBytes-lateSuccessReserve {
			t.Fatalf("failure entry %d: outcome=%v len=%d, want to stay %d under the cap", i, outcome, len(got), lateSuccessReserve)
		}
		existing = got
		if i > 20 {
			t.Fatal("failure entries never ran out of room")
		}
	}
	// Two distinct longest success entries still append whole into the
	// reserved bytes.
	for n, d := range [][32]byte{{0xff}, {0xfe}} {
		var outcome lateEvidenceOutcome
		existing, outcome = appendLateEvidence(existing, lateReportFingerprint(true, d, ""), lateSuccessBody(d, false), true)
		if outcome != lateEvidenceAppended || len(existing) > maxOutcomeSummaryBytes {
			t.Fatalf("success entry %d: outcome=%v len=%d, want appended within the cap", n, outcome, len(existing))
		}
		if want := " " + lateSuccessBody(d, false); !strings.HasSuffix(existing, want) {
			t.Fatalf("success entry %d was truncated: %q", n, existing[len(existing)-80:])
		}
	}
}
