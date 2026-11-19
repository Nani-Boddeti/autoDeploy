package state

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// Slice 22-D record fields: pre_stop_route_digest and not_switched.

func stsStopSent(id int64) PendingActivation {
	p := stsAuthorized(id)
	p.PredecessorStopSent = true
	p.PreStopRouteDigest = [32]byte{0x11}
	return p
}

func notSwitchedResult(id int64) PendingActivation {
	p := stsStopSent(id)
	p.Phase, p.FailureSummary, p.NotSwitched = PhaseResultReady, "candidate failed health gate", true
	return p
}

// A record written by the previous binary (a stop-then-start failure carrying
// predecessor_stop_sent, none of the new keys) must decode unchanged and
// re-encode to the very same bytes.
func TestPreNotSwitchedRecordBytesDecodeAndReEncodeUnchanged(t *testing.T) {
	zero := strings.Repeat("0", 64)
	old := `{"attempt_id":7,"fence":3,"environment_id":"env_a","operation_id":"9001",` +
		`"route_digest":"ab` + strings.Repeat("0", 62) + `","image_digest":"cd` + strings.Repeat("0", 62) + `","route_revision":5,` +
		`"phase":"result_ready","succeeded":false,"observed_route_digest":"` + zero + `","failure_summary":"activation failed at stop-then-start: x",` +
		`"created_at":"2026-11-13T01:02:03Z","updated_at":"2026-11-13T01:02:04Z",` +
		`"release_id":"rel_new","backend_runtime_instance_id":"rt_new",` +
		`"predecessor_release_id":"rel_old","predecessor_runtime_instance_id":"rt_old","drain_seconds":30,` +
		`"predecessor_stop_sent":true}`
	got, err := unmarshalActivation([]byte(old))
	if err != nil {
		t.Fatalf("old record rejected: %v", err)
	}
	if got.NotSwitched || got.PreStopRouteDigest != ([32]byte{}) || !got.PredecessorStopSent || got.Succeeded {
		t.Fatalf("old record decoded with new-field state: %+v", got)
	}
	again, err := got.marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != old {
		t.Fatalf("re-encoding changed an old record:\n got: %s\nwant: %s", again, old)
	}
}

// A new-binary record that does not use the new fields byte-matches the old
// encoding (the keys are omitted, not written empty).
func TestRecordWithoutNewFieldsOmitsThem(t *testing.T) {
	for _, p := range []PendingActivation{authorized(1), stsAuthorized(2), failed(3), succeeded(4)} {
		p.CreatedAt, p.UpdatedAt = time.Now(), time.Now()
		b, err := p.marshal()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "pre_stop_route_digest") || strings.Contains(string(b), "not_switched") {
			t.Fatalf("unset new fields must be omitted: %s", b)
		}
	}
}

func TestNotSwitchedRecordRoundTripAndRestart(t *testing.T) {
	root, a := newActs(t)
	if err := a.SaveActivation(stsStopSent(1)); err != nil {
		t.Fatal(err)
	}
	r := notSwitchedResult(1)
	if err := a.SaveActivation(r); err != nil {
		t.Fatal(err)
	}
	s, err := New(root) // a restart: a fresh store over the same directory
	if err != nil {
		t.Fatal(err)
	}
	a2, err := s.Activations()
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := a2.LoadActivation(1)
	if err != nil || !ok {
		t.Fatalf("err=%v ok=%v", err, ok)
	}
	if !got.NotSwitched || got.PreStopRouteDigest != r.PreStopRouteDigest || got.Succeeded {
		t.Fatalf("got %+v", got)
	}
}

func TestNotSwitchedValidation(t *testing.T) {
	_, a := newActs(t)
	cases := map[string]func(*PendingActivation){
		"on an authorized record": func(p *PendingActivation) { p.Phase, p.FailureSummary = PhaseAuthorized, "" },
		"on a success": func(p *PendingActivation) {
			p.FailureSummary, p.Succeeded, p.ObservedRouteDigest = "", true, [32]byte{1}
		},
		"without a snapshot": func(p *PendingActivation) { p.PreStopRouteDigest = [32]byte{} },
	}
	for name, mut := range cases {
		p := notSwitchedResult(10)
		mut(&p)
		if err := a.SaveActivation(p); !errors.Is(err, ErrInvalidActivation) {
			t.Errorf("%s: err = %v, want ErrInvalidActivation", name, err)
		}
	}
	// A snapshot requires the stop intent.
	p := stsAuthorized(11)
	p.PreStopRouteDigest = [32]byte{1}
	if err := a.SaveActivation(p); !errors.Is(err, ErrInvalidActivation) {
		t.Errorf("snapshot without stop sent: err = %v", err)
	}
}

func TestNotSwitchedAndSnapshotAreImmutableOnceResultReady(t *testing.T) {
	_, a := newActs(t)
	if err := a.SaveActivation(notSwitchedResult(1)); err != nil {
		t.Fatal(err)
	}
	// An identical rewrite is allowed.
	if err := a.SaveActivation(notSwitchedResult(1)); err != nil {
		t.Fatalf("identical rewrite refused: %v", err)
	}
	plain := notSwitchedResult(1)
	plain.NotSwitched = false
	if err := a.SaveActivation(plain); !errors.Is(err, ErrActivationImmutable) {
		t.Fatalf("not_switched true -> false through Save: err = %v", err)
	}
	moved := notSwitchedResult(1)
	moved.PreStopRouteDigest = [32]byte{0x22}
	if err := a.SaveActivation(moved); !errors.Is(err, ErrActivationImmutable) {
		t.Fatalf("snapshot change after result: err = %v", err)
	}

	// A plain failure can never be upgraded to not_switched.
	if err := a.SaveActivation(func() PendingActivation {
		p := stsStopSent(2)
		p.Phase, p.FailureSummary = PhaseResultReady, "x"
		return p
	}()); err != nil {
		t.Fatal(err)
	}
	up := notSwitchedResult(2)
	up.FailureSummary = "x"
	if err := a.SaveActivation(up); !errors.Is(err, ErrActivationImmutable) {
		t.Fatalf("plain -> not_switched: err = %v", err)
	}
}

func TestSnapshotSetOnceBeforeResult(t *testing.T) {
	_, a := newActs(t)
	p := stsAuthorized(1)
	p.PredecessorStopSent = true
	p.PreStopRouteDigest = [32]byte{0x11}
	// Set together with the stop intent (zero -> value) on an existing record.
	base := stsAuthorized(1)
	if err := a.SaveActivation(base); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveActivation(p); err != nil {
		t.Fatalf("zero -> value refused: %v", err)
	}
	p.PreStopRouteDigest = [32]byte{0x22}
	if err := a.SaveActivation(p); !errors.Is(err, ErrActivationImmutable) {
		t.Fatalf("value -> other value: err = %v", err)
	}
	// Once a result exists a zero snapshot cannot gain a value.
	r := stsAuthorized(2)
	r.PredecessorStopSent = true
	r.Phase, r.FailureSummary = PhaseResultReady, "x"
	if err := a.SaveActivation(r); err != nil {
		t.Fatal(err)
	}
	r.PreStopRouteDigest = [32]byte{0x11}
	if err := a.SaveActivation(r); !errors.Is(err, ErrActivationImmutable) {
		t.Fatalf("snapshot added after result: err = %v", err)
	}
}

func TestDowngradeNotSwitchedIsDurableAndIdempotent(t *testing.T) {
	root, a := newActs(t)
	rec := notSwitchedResult(1)
	if err := a.SaveActivation(rec); err != nil {
		t.Fatal(err)
	}
	if err := a.DowngradeNotSwitched(1); err != nil {
		t.Fatal(err)
	}
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := s.Activations()
	got, ok, err := a2.LoadActivation(1)
	if err != nil || !ok {
		t.Fatalf("err=%v ok=%v", err, ok)
	}
	if got.NotSwitched || got.FailureSummary != rec.FailureSummary || got.Succeeded || got.PreStopRouteDigest != rec.PreStopRouteDigest || got.Phase != PhaseResultReady {
		t.Fatalf("downgraded record = %+v", got)
	}
	// On disk it carries no not_switched key: a plain failure again.
	b, err := os.ReadFile(a2.path(1))
	if err != nil || strings.Contains(string(b), "not_switched") {
		t.Fatalf("file still names not_switched: %s err=%v", b, err)
	}
	// Idempotent, a missing record is a no-op, and it never turns the outcome on.
	if err := a2.DowngradeNotSwitched(1); err != nil {
		t.Fatalf("second downgrade: %v", err)
	}
	if err := a2.DowngradeNotSwitched(99); err != nil {
		t.Fatalf("missing record: %v", err)
	}
	if err := a2.DowngradeNotSwitched(0); !errors.Is(err, ErrInvalidActivation) {
		t.Fatalf("bad id: %v", err)
	}
	if err := (ActivationStore{}).DowngradeNotSwitched(1); err == nil {
		t.Fatal("zero-value store must fail")
	}
	// The executor re-saving the outcome after the downgrade is refused.
	if err := a2.SaveActivation(rec); !errors.Is(err, ErrActivationImmutable) {
		t.Fatalf("re-save of not_switched after downgrade: err = %v", err)
	}
}

func TestMalformedPreStopDigestFailsClosed(t *testing.T) {
	p := stsStopSent(1)
	p.CreatedAt, p.UpdatedAt = time.Now(), time.Now()
	b, _ := p.marshal()
	bad := strings.Replace(string(b), `"pre_stop_route_digest":"11`, `"pre_stop_route_digest":"zz`, 1)
	if _, err := unmarshalActivation([]byte(bad)); err == nil {
		t.Fatal("malformed digest accepted")
	}
	zero := strings.Replace(string(b), `"pre_stop_route_digest":"11`+strings.Repeat("0", 62), `"pre_stop_route_digest":"`+strings.Repeat("0", 64), 1)
	if _, err := unmarshalActivation([]byte(zero)); err == nil {
		t.Fatal("explicit all-zero snapshot accepted")
	}
}
