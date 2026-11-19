package secret

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"autodeploy/internal/crypto"
)

type verifyRepo struct {
	generation string
	versions   []Version
	listCalls  int
	cursors    []string
}

func (r *verifyRepo) DatabaseGeneration(context.Context) (string, error) { return r.generation, nil }
func (r *verifyRepo) ListDecryptableVersions(_ context.Context, cursor string, limit int) ([]Version, error) {
	r.listCalls++
	r.cursors = append(r.cursors, cursor)
	var page []Version
	for _, v := range r.versions {
		if v.ID > cursor && len(page) < limit {
			page = append(page, v)
		}
	}
	return page, nil
}

type noReserve struct{ t *testing.T }

func (n noReserve) ReserveWrapInvocation(string) error {
	n.t.Fatal("verify reserved a wrap invocation")
	return nil
}

func verifyFixture(t *testing.T, count int) (*verifyRepo, crypto.KeyRing) {
	t.Helper()
	ring, err := crypto.NewKeyRing("kek-a", map[string][]byte{"kek-a": bytes.Repeat([]byte{1}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	repo := &verifyRepo{generation: "0123456789abcdef"}
	for i := 0; i < count; i++ {
		v := Version{ID: fmt.Sprintf("version-%04d", i), Scope: Scope{OwnerID: "o", ProjectID: "p", EnvironmentID: "e", SecretID: fmt.Sprintf("secret-%d", i), Purpose: "runtime", DeliveryScope: "runtime"}}
		env, err := crypto.Encrypt(ring, &okReserver{}, cryptoScope(v), []byte("value"))
		if err != nil {
			t.Fatal(err)
		}
		v.Envelope = env
		repo.versions = append(repo.versions, v)
	}
	return repo, ring
}

type okReserver struct{}

func (okReserver) ReserveWrapInvocation(string) error { return nil }

func TestVerifyGenerationMismatchFailsBeforeAnyRead(t *testing.T) {
	repo, ring := verifyFixture(t, 2)
	_, err := NewVerifier(repo, ring).Verify(context.Background(), "ffffffffffffffff")
	if !errors.Is(err, ErrGenerationMismatch) || repo.listCalls != 0 {
		t.Fatalf("err=%v listCalls=%d", err, repo.listCalls)
	}
}

func TestVerifyPagesAllVersionsAndClassifiesFailures(t *testing.T) {
	repo, ring := verifyFixture(t, 250)
	repo.versions[3].Envelope.Ciphertext[0] ^= 1
	repo.versions[120].Envelope.WrappedDEK[0] ^= 1
	repo.versions[200].Envelope.KEKID = "kek-gone"
	res, err := NewVerifier(repo, ring).Verify(context.Background(), repo.generation)
	if err != nil || res.Checked != 250 || res.Failed != 3 || len(res.Failures) != 3 {
		t.Fatalf("%+v %v", res, err)
	}
	want := []VerifyFailure{{"version-0003", "kek-a", VerifyDecryptFailed}, {"version-0120", "kek-a", VerifyUnwrapFailed}, {"version-0200", "kek-gone", VerifyKEKMissing}}
	for i, w := range want {
		if res.Failures[i] != w {
			t.Fatalf("failure %d = %+v want %+v", i, res.Failures[i], w)
		}
	}
	if repo.listCalls != 3 || repo.cursors[1] != "version-0099" || repo.cursors[2] != "version-0199" {
		t.Fatalf("paging calls=%d cursors=%v", repo.listCalls, repo.cursors)
	}
}

func TestVerifyNeedsNoReserver(t *testing.T) {
	// Structural proof: NewVerifier takes only a VerifyRepository and a
	// KeyProvider; neither can reserve wrap invocations or write.
	var _ VerifyRepository = (*verifyRepo)(nil)
	var _ crypto.WrapInvocationReserver = noReserve{}
	repo, ring := verifyFixture(t, 1)
	if res, err := NewVerifier(repo, ring).Verify(context.Background(), repo.generation); err != nil || res.Checked != 1 || res.Failed != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

type shortKey struct{}

func (shortKey) Active() (string, []byte, error) { return "kek-a", make([]byte, 31), nil }
func (shortKey) Resolve(string) ([]byte, error)  { return make([]byte, 31), nil }

func TestVerifyMalformedKEKIsKekInvalid(t *testing.T) {
	repo, _ := verifyFixture(t, 1)
	res, err := NewVerifier(repo, shortKey{}).Verify(context.Background(), repo.generation)
	if err != nil || res.Failed != 1 || res.Failures[0].Reason != VerifyKEKInvalid {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestVerifyFailureListCapKeepsExactCount(t *testing.T) {
	old := maxFailuresListed
	maxFailuresListed = 5
	defer func() { maxFailuresListed = old }()
	repo, ring := verifyFixture(t, 12)
	for i := range repo.versions {
		repo.versions[i].Envelope.Ciphertext[0] ^= 1
	}
	res, err := NewVerifier(repo, ring).Verify(context.Background(), repo.generation)
	if err != nil || res.Checked != 12 || res.Failed != 12 || len(res.Failures) != 5 {
		t.Fatalf("%+v %v", res, err)
	}
}
