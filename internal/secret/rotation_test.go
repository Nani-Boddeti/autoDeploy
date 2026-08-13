package secret

import (
	"context"
	"errors"
	"testing"

	"autodeploy/internal/crypto"
)

type rotationProvider struct {
	active string
	keys   map[string][]byte
}

func (p rotationProvider) Active() (string, []byte, error) {
	return p.active, append([]byte(nil), p.keys[p.active]...), nil
}
func (p rotationProvider) Resolve(id string) ([]byte, error) {
	key, ok := p.keys[id]
	if !ok {
		return nil, crypto.ErrUnknownKey
	}
	return append([]byte(nil), key...), nil
}

type rotationRepo struct {
	versions     []Version
	reservations int
	conflict     string
	count        int64
	lastLimit    int
}

func (r *rotationRepo) ReserveWrapInvocation(string) error { r.reservations++; return nil }
func (r *rotationRepo) ListVersionsByKEK(_ context.Context, id, cursor string, n int) ([]Version, error) {
	r.lastLimit = n
	out := []Version{}
	for _, v := range r.versions {
		if v.Envelope.KEKID == id && v.ID > cursor {
			out = append(out, v)
			if len(out) == n {
				break
			}
		}
	}
	return out, nil
}
func (r *rotationRepo) Rewrap(_ context.Context, _ Scope, id string, revision uint64, e crypto.Envelope) error {
	if id == r.conflict {
		return errors.New("conflict")
	}
	for i := range r.versions {
		if r.versions[i].ID == id && r.versions[i].EnvelopeRevision == revision {
			r.versions[i].Envelope = e
			r.versions[i].EnvelopeRevision++
			return nil
		}
	}
	return errors.New("conflict")
}
func (r *rotationRepo) CountVersionsByKEK(context.Context, string) (int64, error) {
	return r.count, nil
}
func rotationVersion(t *testing.T, id, kek string) Version {
	t.Helper()
	scope := testScope()
	p := rotationProvider{active: kek, keys: map[string][]byte{kek: []byte("01234567890123456789012345678901")}}
	e, err := crypto.Encrypt(p, &rotationRepo{}, crypto.Scope{OwnerID: scope.OwnerID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, VariableID: scope.SecretID, SecretVersionID: id, Purpose: scope.Purpose, DeliveryScope: scope.DeliveryScope}, []byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	return Version{ID: id, Scope: scope, Number: 1, State: StateActive, Envelope: e, EnvelopeRevision: 1}
}
func TestRotatorBatchesResumeAndBurnsConflictReservations(t *testing.T) {
	old, newKey := []byte("01234567890123456789012345678901"), []byte("abcdefghijklmnopqrstuvwxyz123456")
	_ = old
	_ = newKey
	// Use distinct valid AES-256 keys and a stable ID keyset order.
	p := rotationProvider{active: "new", keys: map[string][]byte{"old": []byte("01234567890123456789012345678901"), "new": []byte("abcdefghijklmnopqrstuvwxyz123456")}}
	r := &rotationRepo{}
	r.versions = []Version{rotationVersionWith(t, "a", "old", p.keys["old"]), rotationVersionWith(t, "b", "new", p.keys["new"]), rotationVersionWith(t, "c", "old", p.keys["old"])}
	before := append([]byte(nil), r.versions[0].Envelope.Ciphertext...)
	r.conflict = "a"
	rot := NewRotator(r, p)
	got, err := rot.RewrapBatch(context.Background(), RewrapRequest{OldKEKID: "old", BatchSize: 1})
	if err != nil || got.Selected != 1 || got.Conflicts != 1 || got.NextCursor != "" || r.reservations != 1 {
		t.Fatalf("first=%+v err=%v reservations=%d", got, err, r.reservations)
	}
	r.conflict = ""
	got, err = rot.RewrapBatch(context.Background(), RewrapRequest{OldKEKID: "old", Cursor: got.NextCursor})
	if err != nil || got.Selected != 2 || got.Rewrapped != 2 || r.versions[2].Envelope.KEKID != "new" || r.versions[0].Envelope.KEKID != "new" {
		t.Fatalf("resume=%+v err=%v", got, err)
	}
	if string(before) != string(r.versions[0].Envelope.Ciphertext) || r.versions[0].ID != "a" || r.versions[0].Number != 1 {
		t.Fatal("rewrap changed immutable value fields")
	}
	if _, err = rot.RewrapBatch(context.Background(), RewrapRequest{OldKEKID: "old", BatchSize: 101}); !errors.Is(err, ErrRotationInput) {
		t.Fatal("max batch accepted")
	}
}
func TestRotatorUsesDefaultBatchSize(t *testing.T) {
	p := rotationProvider{active: "new", keys: map[string][]byte{"old": make([]byte, 32), "new": []byte("abcdefghijklmnopqrstuvwxyz123456")}}
	r := &rotationRepo{}
	if _, err := NewRotator(r, p).RewrapBatch(context.Background(), RewrapRequest{OldKEKID: "old"}); err != nil {
		t.Fatal(err)
	}
	if r.lastLimit != DefaultRewrapBatchSize {
		t.Fatalf("default batch=%d", r.lastLimit)
	}
}
func rotationVersionWith(t *testing.T, id, kek string, key []byte) Version {
	t.Helper()
	scope := testScope()
	p := rotationProvider{active: kek, keys: map[string][]byte{kek: key}}
	e, err := crypto.Encrypt(p, &rotationRepo{}, crypto.Scope{OwnerID: scope.OwnerID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, VariableID: scope.SecretID, SecretVersionID: id, Purpose: scope.Purpose, DeliveryScope: scope.DeliveryScope}, []byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	return Version{ID: id, Scope: scope, Number: 1, State: StateActive, Envelope: e, EnvelopeRevision: 1}
}
func TestRotatorRetirementGuard(t *testing.T) {
	r := &rotationRepo{count: 1}
	p := rotationProvider{active: "new", keys: map[string][]byte{"new": make([]byte, 32)}}
	if err := NewRotator(r, p).CanRetire(context.Background(), "old"); !errors.Is(err, ErrRetirement) {
		t.Fatalf("retire=%v", err)
	}
	r.count = 0
	if err := NewRotator(r, p).CanRetire(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
	if err := NewRotator(r, p).CanRetire(context.Background(), "new"); !errors.Is(err, ErrRetirement) {
		t.Fatalf("active retire=%v", err)
	}
}
