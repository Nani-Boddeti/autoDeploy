package docker

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const stopTestID = "abc123def456"

func stopClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	c, err := newClient(startFakeEngine(t, h))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return c
}

func TestStopContainerSuccessStatuses(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusNotModified} {
		var hits atomic.Int32
		var gotMethod, gotPath, gotT string
		c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			gotMethod, gotPath, gotT = r.Method, r.URL.Path, r.URL.Query().Get("t")
			w.WriteHeader(status)
		}))
		if err := c.StopContainer(context.Background(), stopTestID, 7*time.Second); err != nil {
			t.Fatalf("status %d: %v", status, err)
		}
		if hits.Load() != 1 || gotMethod != http.MethodPost || gotPath != "/v1.44/containers/"+stopTestID+"/stop" || gotT != "7" {
			t.Fatalf("status %d: got %d %s %s t=%q", status, hits.Load(), gotMethod, gotPath, gotT)
		}
	}
}

func TestStopContainerNotFound(t *testing.T) {
	c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
	}))
	err := c.StopContainer(context.Background(), stopTestID, time.Second)
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("err = %v, want ErrContainerNotFound", err)
	}
}

func TestInspectContainer404IsNotFoundAndStillAnEngineError(t *testing.T) {
	c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
	}))
	_, err := c.InspectContainer(context.Background(), stopTestID)
	var ee *EngineError
	if !errors.Is(err, ErrContainerNotFound) || !errors.As(err, &ee) || ee.Status != http.StatusNotFound || ee.Class != ErrClassConfiguration {
		t.Fatalf("err = %v", err)
	}
}

func TestClientHasNoRemoveOrKillMethod(t *testing.T) {
	typ := reflect.TypeOf(&Client{})
	for i := 0; i < typ.NumMethod(); i++ {
		if n := typ.Method(i).Name; strings.HasPrefix(n, "Remove") || strings.HasPrefix(n, "Kill") {
			t.Fatalf("client has forbidden method %s", n)
		}
	}
}

func TestStopContainer500IsBoundedAndSanitized(t *testing.T) {
	c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom\x1b[31m" + strings.Repeat("x", 100000)))
	}))
	err := c.StopContainer(context.Background(), stopTestID, time.Second)
	var ee *EngineError
	if !errors.As(err, &ee) || ee.Status != http.StatusInternalServerError || ee.Class != ErrClassTransient {
		t.Fatalf("err = %v", err)
	}
	if len(err.Error()) > 1024 || strings.ContainsRune(err.Error(), 0x1b) {
		t.Fatalf("error not bounded/sanitized: %d bytes", len(err.Error()))
	}
}

func TestStopContainerInvalidInputNeverReachesSocket(t *testing.T) {
	var hits atomic.Int32
	c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, id := range []string{"", "../x", "a/b", "a?t=0", "a b", "..", "abc123", "ABC123DEF456", "my-container",
		containerNamePrefix, containerNamePrefix + "a/b", containerNamePrefix + "../x", containerNamePrefix + strings.Repeat("a", 242)} {
		if err := c.StopContainer(context.Background(), id, time.Second); err == nil {
			t.Fatalf("stop id %q: want error", id)
		}
		if _, err := c.WaitStopped(context.Background(), id, time.Second, 10*time.Millisecond); err == nil {
			t.Fatalf("wait id %q: want error", id)
		}
	}
	for _, g := range []time.Duration{0, -time.Second, 301 * time.Second, 1500 * time.Millisecond} {
		if err := c.StopContainer(context.Background(), stopTestID, g); err == nil {
			t.Fatalf("grace %s: want error", g)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("%d requests reached the socket", hits.Load())
	}
}

func TestStopContainerDeadlineExceedsGraceButHonorsCtx(t *testing.T) {
	t.Parallel()
	// The default 5s timeout would kill a 6s call; the grace-derived
	// deadline must let it finish.
	c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5500 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	if err := c.StopContainer(context.Background(), stopTestID, 6*time.Second); err != nil {
		t.Fatalf("slow stop within grace: %v", err)
	}
}

func TestStopContainerCtxCancelAbortsPromptly(t *testing.T) {
	c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	err := c.StopContainer(ctx, stopTestID, 60*time.Second)
	if !errors.Is(err, context.Canceled) || time.Since(start) > 2*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
}

func TestStopContainer409IsConfiguration(t *testing.T) {
	c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "conflict", http.StatusConflict)
	}))
	err := c.StopContainer(context.Background(), stopTestID, time.Second)
	var ee *EngineError
	if !errors.As(err, &ee) || ee.Status != http.StatusConflict || ee.Class != ErrClassConfiguration {
		t.Fatalf("err = %v", err)
	}
}

func TestStopContainerUnexpectedOKIsMalformed(t *testing.T) {
	c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	err := c.StopContainer(context.Background(), stopTestID, time.Second)
	if err == nil || engineErrorClass(t, err) != ErrClassMalformed {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitStoppedFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"no state": `{"Id":"abc123def456"}`,
		"paused":   `{"Id":"abc123def456","State":{"Status":"paused","Running":false,"Paused":true}}`,
		"unknown":  `{"Id":"abc123def456","State":{"Status":"weird","Running":false}}`,
	} {
		c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		_, err := c.WaitStopped(context.Background(), stopTestID, 150*time.Millisecond, 20*time.Millisecond)
		if !errors.Is(err, ErrStopTimeout) {
			t.Fatalf("%s: err = %v, want ErrStopTimeout", name, err)
		}
	}
}

func TestWaitStoppedDeadAndCreatedAreStopped(t *testing.T) {
	for _, status := range []string{"dead", "created", "removing"} {
		c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(inspectBody(status, false)))
		}))
		if _, err := c.WaitStopped(context.Background(), stopTestID, time.Second, 10*time.Millisecond); err != nil {
			t.Fatalf("%s: %v", status, err)
		}
	}
}

func inspectBody(status string, running bool) string {
	r := "false"
	if running {
		r = "true"
	}
	return `{"Id":"abc123def456","State":{"Status":"` + status + `","Running":` + r + `}}`
}

func TestWaitStoppedRunningThenExited(t *testing.T) {
	var n atomic.Int32
	c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.Write([]byte(inspectBody("running", true)))
			return
		}
		w.Write([]byte(inspectBody("exited", false)))
	}))
	got, err := c.WaitStopped(context.Background(), stopTestID, 5*time.Second, 10*time.Millisecond)
	if err != nil || got.State.Status != "exited" || n.Load() != 3 {
		t.Fatalf("got %+v err %v polls %d", got.State, err, n.Load())
	}
}

func TestWaitStoppedRestartingIsNotStopped(t *testing.T) {
	var n atomic.Int32
	c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 2 {
			w.Write([]byte(`{"Id":"abc123def456","State":{"Status":"restarting","Running":false,"Restarting":true}}`))
			return
		}
		w.Write([]byte(inspectBody("exited", false)))
	}))
	if _, err := c.WaitStopped(context.Background(), stopTestID, 5*time.Second, 10*time.Millisecond); err != nil || n.Load() != 2 {
		t.Fatalf("err %v polls %d", err, n.Load())
	}
}

func TestWaitStoppedTimeout(t *testing.T) {
	c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(inspectBody("running", true)))
	}))
	start := time.Now()
	_, err := c.WaitStopped(context.Background(), stopTestID, 200*time.Millisecond, 20*time.Millisecond)
	if !errors.Is(err, ErrStopTimeout) || !strings.Contains(err.Error(), "running") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
}

func TestWaitStoppedNotFoundMidWait(t *testing.T) {
	var n atomic.Int32
	c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 2 {
			w.Write([]byte(inspectBody("running", true)))
			return
		}
		http.Error(w, "gone", http.StatusNotFound)
	}))
	_, err := c.WaitStopped(context.Background(), stopTestID, 5*time.Second, 10*time.Millisecond)
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitStoppedCtxCancelPrompt(t *testing.T) {
	c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(inspectBody("running", true)))
	}))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := c.WaitStopped(ctx, stopTestID, 30*time.Second, 5*time.Second)
	if !errors.Is(err, context.Canceled) || time.Since(start) > 2*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
}

func TestWaitStoppedInvalidArgs(t *testing.T) {
	c := stopClient(t, http.NotFoundHandler())
	if _, err := c.WaitStopped(context.Background(), stopTestID, 0, time.Second); err == nil {
		t.Fatal("want error for zero timeout")
	}
	if _, err := c.WaitStopped(context.Background(), stopTestID, time.Second, 0); err == nil {
		t.Fatal("want error for zero interval")
	}
}

func TestStopContainerAcceptsIDsAndContainerNames(t *testing.T) {
	long, err := ContainerName(strings.Repeat("a", 128))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{strings.Repeat("a", 12), strings.Repeat("0f", 32), long, containerNamePrefix + "42"} {
		var path string
		c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		}))
		if err := c.StopContainer(context.Background(), id, time.Second); err != nil {
			t.Fatalf("id %q: %v", id, err)
		}
		if path != "/v1.44/containers/"+id+"/stop" {
			t.Fatalf("path = %q", path)
		}
	}
}
