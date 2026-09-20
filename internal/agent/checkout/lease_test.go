package checkout

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"autodeploy/internal/githubapp"
)

type fakeClient struct {
	beginResult Reservation
	beginErr    error
	beginCalls  int

	startResult Credential
	startErr    error
	startCalls  int

	closeErr   error
	closeCalls int
}

func (f *fakeClient) CheckoutBegin(_ context.Context, _, _ int64) (Reservation, error) {
	f.beginCalls++
	return f.beginResult, f.beginErr
}
func (f *fakeClient) CheckoutStart(_ context.Context, _, _ int64, _ string) (Credential, error) {
	f.startCalls++
	return f.startResult, f.startErr
}
func (f *fakeClient) CheckoutClose(_ context.Context, _, _ int64, _ string) error {
	f.closeCalls++
	return f.closeErr
}

func testBinding() githubapp.CheckoutBinding {
	return githubapp.CheckoutBinding{
		OwnerID: "owner", ProjectID: "project", InstallationID: 1, RepositoryID: 201,
		CloneURL: "https://github.com/owner/repo.git", CommitSHA: "0123456789abcdef0123456789abcdef01234567", ReservationID: "reservation-1",
	}
}

func TestBeginReservesAndPopulatesBinding(t *testing.T) {
	client := &fakeClient{beginResult: Reservation{ReservationID: "reservation-1", Binding: testBinding()}}
	lease, err := Begin(context.Background(), client, 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	if client.beginCalls != 1 {
		t.Fatalf("beginCalls=%d, want 1", client.beginCalls)
	}
	if lease.Binding().ReservationID != "reservation-1" || lease.Binding().CommitSHA == "" {
		t.Fatalf("binding=%+v", lease.Binding())
	}
}

func TestBeginFailsClosedWithoutReservationID(t *testing.T) {
	client := &fakeClient{beginResult: Reservation{}}
	if _, err := Begin(context.Background(), client, 7, 3); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v, want ErrUnavailable for an empty reservation id", err)
	}
}

// TestBrokerAndWriterFailClosedBeforeStart proves the requirement directly:
// anything needing the token must fail closed before Start, never returning
// a broker or writer with no redaction behind it.
func TestBrokerAndWriterFailClosedBeforeStart(t *testing.T) {
	client := &fakeClient{beginResult: Reservation{ReservationID: "reservation-1", Binding: testBinding()}}
	lease, err := Begin(context.Background(), client, 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lease.NewAskPassBroker(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("NewAskPassBroker before Start: err=%v, want ErrUnavailable", err)
	}
	var buf bytes.Buffer
	if _, err := lease.RedactingWriter(&buf); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("RedactingWriter before Start: err=%v, want ErrUnavailable", err)
	}
}

func TestStartDeliversTokenThroughAskPassBroker(t *testing.T) {
	client := &fakeClient{
		beginResult: Reservation{ReservationID: "reservation-1", Binding: testBinding()},
		startResult: Credential{Token: "installation-token-value", ExpiresAt: time.Now().Add(time.Hour)},
	}
	lease, err := Begin(context.Background(), client, 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lease.ExpiresAt().IsZero() {
		t.Fatal("ExpiresAt must be populated after Start")
	}

	broker, err := lease.NewAskPassBroker()
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	reader := broker.Reader()
	if reader == nil {
		t.Fatal("broker had no reader")
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "installation-token-value\n" {
		t.Fatalf("askpass payload=%q", data)
	}

	// One-use: a second broker must fail closed.
	if _, err := lease.NewAskPassBroker(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("second NewAskPassBroker: err=%v, want ErrUnavailable", err)
	}
}

func TestStartFailsClosedWithoutToken(t *testing.T) {
	client := &fakeClient{
		beginResult: Reservation{ReservationID: "reservation-1", Binding: testBinding()},
		startResult: Credential{},
	}
	lease, err := Begin(context.Background(), client, 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Start(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v, want ErrUnavailable for an empty token", err)
	}
}

func TestRedactingWriterRedactsAfterStart(t *testing.T) {
	client := &fakeClient{
		beginResult: Reservation{ReservationID: "reservation-1", Binding: testBinding()},
		startResult: Credential{Token: "installation-token-value", ExpiresAt: time.Now().Add(time.Hour)},
	}
	lease, err := Begin(context.Background(), client, 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := lease.RedactingWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("output containing installation-token-value inline")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if bytesContains(buf.Bytes(), "installation-token-value") {
		t.Fatalf("token leaked through redacting writer: %s", buf.String())
	}
}

func bytesContains(haystack []byte, needle string) bool {
	return bytes.Contains(haystack, []byte(needle))
}

func TestCloseIsIdempotentAndCallsClient(t *testing.T) {
	client := &fakeClient{beginResult: Reservation{ReservationID: "reservation-1", Binding: testBinding()}}
	lease, err := Begin(context.Background(), client, 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(context.Background()); err != nil {
		t.Fatalf("second close: %v, want idempotent success", err)
	}
	if client.closeCalls != 1 {
		t.Fatalf("closeCalls=%d, want exactly 1 (idempotent, not re-dispatched)", client.closeCalls)
	}
}

func TestStartFailsAfterClose(t *testing.T) {
	client := &fakeClient{
		beginResult: Reservation{ReservationID: "reservation-1", Binding: testBinding()},
		startResult: Credential{Token: "installation-token-value", ExpiresAt: time.Now().Add(time.Hour)},
	}
	lease, err := Begin(context.Background(), client, 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := lease.Start(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Start after Close: err=%v, want ErrUnavailable", err)
	}
}

func TestSetCancelStoresHookWithoutPanicking(t *testing.T) {
	client := &fakeClient{beginResult: Reservation{ReservationID: "reservation-1", Binding: testBinding()}}
	lease, err := Begin(context.Background(), client, 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	lease.SetCancel(func() { called = true })
	lease.cancel()
	if !called {
		t.Fatal("SetCancel's hook was not stored/invoked")
	}
}
