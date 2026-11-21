package admincli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
)

type fakeLocalRepo struct {
	ownerErrs []error // consumed one per LocalOwner call; then success
	calls     int
	enrollErr error
	enrolled  bool
	requests  int
}

func (f *fakeLocalRepo) LocalOwner(context.Context) (postgres.LocalOwner, error) {
	f.calls++
	if len(f.ownerErrs) > 0 {
		err := f.ownerErrs[0]
		f.ownerErrs = f.ownerErrs[1:]
		return postgres.LocalOwner{}, err
	}
	return postgres.LocalOwner{OwnerID: "o", UserID: "u"}, nil
}
func (f *fakeLocalRepo) EnrollLocalServer(_ context.Context, _, _, newID string, _ [32]byte, _ int) (postgres.LocalEnrollment, error) {
	f.requests++
	return postgres.LocalEnrollment{ServerID: newID, Enrolled: f.enrolled}, f.enrollErr
}

func noSleep(context.Context, time.Duration) error { return nil }

func TestEnrollLocalWaitPollsThenRefusesWhenNoAdministrator(t *testing.T) {
	repo := &fakeLocalRepo{ownerErrs: []error{postgres.ErrNoAdministrator, postgres.ErrNoAdministrator, postgres.ErrNoAdministrator}}
	var out bytes.Buffer
	opts := LocalEnrollOptions{Name: "box", TokenFile: filepath.Join(t.TempDir(), "t"), Wait: 0, Sleep: noSleep}
	if _, err := EnrollLocal(context.Background(), repo, opts, &out); !errors.Is(err, ErrAdministratorMissing) || repo.calls != 1 {
		t.Fatalf("no wait: calls=%d err=%v", repo.calls, err)
	}
	repo = &fakeLocalRepo{ownerErrs: []error{postgres.ErrNoAdministrator, postgres.ErrNoAdministrator}}
	opts.Wait = time.Minute
	if _, err := EnrollLocal(context.Background(), repo, opts, &out); err != nil || repo.calls != 3 || repo.requests != 1 {
		t.Fatalf("wait: calls=%d requests=%d err=%v", repo.calls, repo.requests, err)
	}
}

func TestEnrollLocalTokenOnlyInFileNeverInOutputOrErrors(t *testing.T) {
	repo := &fakeLocalRepo{}
	path := filepath.Join(t.TempDir(), "t")
	var out bytes.Buffer
	if _, err := EnrollLocal(context.Background(), repo, LocalEnrollOptions{Name: "box", TokenFile: path}, &out); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	token := strings.TrimSpace(string(raw))
	if _, err := server.ParseEnrollmentToken(token); err != nil || strings.Contains(out.String(), token) {
		t.Fatalf("token file invalid or token on stdout: %v", err)
	}
	if info, _ := os.Lstat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	// A failed registration removes the file this call created and reports no token.
	path2 := filepath.Join(t.TempDir(), "t2")
	repo = &fakeLocalRepo{enrollErr: errors.New("db detail")}
	_, err := EnrollLocal(context.Background(), repo, LocalEnrollOptions{Name: "box", TokenFile: path2}, &out)
	if !errors.Is(err, ErrOperationFailed) || strings.Contains(err.Error(), "db detail") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Lstat(path2); !os.IsNotExist(statErr) {
		t.Fatal("failed run left the token file")
	}
}

func TestEnrollLocalRejectsInvalidInputBeforeAnyWork(t *testing.T) {
	repo := &fakeLocalRepo{}
	for _, opts := range []LocalEnrollOptions{{Name: "", TokenFile: "/x"}, {Name: " pad", TokenFile: "/x"}, {Name: "box"}} {
		if _, err := EnrollLocal(context.Background(), repo, opts, &bytes.Buffer{}); !errors.Is(err, ErrOperationFailed) || repo.calls != 0 {
			t.Fatalf("%+v: calls=%d err=%v", opts, repo.calls, err)
		}
	}
}

func TestEnrollLocalDisabledAdministratorIsNotPolled(t *testing.T) {
	repo := &fakeLocalRepo{ownerErrs: []error{postgres.ErrOwnerInactive}}
	opts := LocalEnrollOptions{Name: "box", TokenFile: filepath.Join(t.TempDir(), "t"), Wait: time.Hour, Sleep: noSleep}
	if _, err := EnrollLocal(context.Background(), repo, opts, &bytes.Buffer{}); !errors.Is(err, ErrAdministratorDisabled) || repo.calls != 1 {
		t.Fatalf("calls=%d err=%v", repo.calls, err)
	}
}

func TestEnrollLocalMapsReenrollAndUnavailableAndRemovesFileWhenEnrolled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t")
	repo := &fakeLocalRepo{enrollErr: postgres.ErrServerNeedsReenroll}
	if _, err := EnrollLocal(context.Background(), repo, LocalEnrollOptions{Name: "box", TokenFile: path}, &bytes.Buffer{}); !errors.Is(err, ErrServerNeedsReenroll) {
		t.Fatalf("err=%v", err)
	}
	repo = &fakeLocalRepo{enrollErr: postgres.ErrServerAuthorizationDenied}
	if _, err := EnrollLocal(context.Background(), repo, LocalEnrollOptions{Name: "box", ServerID: "server_x", TokenFile: path}, &bytes.Buffer{}); !errors.Is(err, ErrLocalServerUnavailable) {
		t.Fatalf("err=%v", err)
	}
	repo = &fakeLocalRepo{enrolled: true}
	var out bytes.Buffer
	if enrolled, err := EnrollLocal(context.Background(), repo, LocalEnrollOptions{Name: "box", TokenFile: path}, &out); err != nil || !enrolled || out.Len() == 0 {
		t.Fatalf("enrolled=%v err=%v", enrolled, err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("token file left for an already enrolled server")
	}
}
