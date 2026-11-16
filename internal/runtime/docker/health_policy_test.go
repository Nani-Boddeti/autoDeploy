package docker

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"autodeploy/internal/healthgate"
	"autodeploy/internal/queue"
)

func statusSeq(seq ...ContainerStatus) ContainerStateFunc {
	var i int32
	return func(context.Context) (ContainerStatus, error) {
		n := int(atomic.AddInt32(&i, 1)) - 1
		if n >= len(seq) {
			n = len(seq) - 1
		}
		return seq[n], nil
	}
}

func running(health string) ContainerStatus {
	return ContainerStatus{State: ContainerStateRunning, Health: health}
}

func countingProbe(ok bool, calls *int32) HealthProbeFunc {
	return func(context.Context) (bool, error) {
		atomic.AddInt32(calls, 1)
		return ok, nil
	}
}

func TestGateTunedThresholdHonoured(t *testing.T) {
	for _, n := range []int{1, 5} {
		var calls int32
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		res, err := runHealthGate(ctx, countingProbe(true, &calls), alwaysRunning, testInterval, testTimeout, n)
		cancel()
		if err != nil || !res.Success {
			t.Fatalf("threshold %d: res=%+v err=%v", n, res, err)
		}
		if got := int(atomic.LoadInt32(&calls)); got != n {
			t.Fatalf("threshold %d: probe called %d times", n, got)
		}
	}
}

func TestGateTunedIntervalAndDeadlineHonoured(t *testing.T) {
	var calls int32
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	res, err := runHealthGate(ctx, countingProbe(false, &calls), alwaysRunning, 20*time.Millisecond, testTimeout, 3)
	if err != nil || res.Reason != HealthGateFailureDeadlineExceeded {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if got := atomic.LoadInt32(&calls); got < 2 || got > 4 {
		t.Fatalf("20ms interval over 60ms probed %d times", got)
	}
}

func TestGateUnhealthyFailsImmediately(t *testing.T) {
	var calls int32
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	res, err := runHealthGate(ctx, countingProbe(true, &calls), statusSeq(running("unhealthy")), testInterval, testTimeout, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.Success || res.Reason != HealthGateFailureContainerUnhealthy {
		t.Fatalf("res = %+v", res)
	}
	if time.Since(start) > time.Second {
		t.Fatal("unhealthy must fail before the deadline")
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("no probe should count after an unhealthy status")
	}
}

func TestGateStartingBlocksPassUntilHealthy(t *testing.T) {
	var calls int32
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Five rounds of "starting" with HTTP ok, then healthy.
	st := statusSeq(running("starting"), running("starting"), running("starting"), running("starting"), running("starting"), running("healthy"))
	res, err := runHealthGate(ctx, countingProbe(true, &calls), st, testInterval, testTimeout, 3)
	if err != nil || !res.Success {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	// Starting rounds never count, so success needs 3 healthy rounds after.
	if got := atomic.LoadInt32(&calls); got < 8 {
		t.Fatalf("probe called %d times; starting rounds must not count toward the streak", got)
	}
}

func TestGateStartingForeverHitsDeadline(t *testing.T) {
	var calls int32
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	res, _ := runHealthGate(ctx, countingProbe(true, &calls), statusSeq(running("starting")), testInterval, testTimeout, 1)
	if res.Success || res.Reason != HealthGateFailureDeadlineExceeded {
		t.Fatalf("res = %+v", res)
	}
}

func TestGateNoneAndHealthyHaveNoEffect(t *testing.T) {
	for _, h := range []string{"", "none", "healthy"} {
		var calls int32
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		res, err := runHealthGate(ctx, countingProbe(true, &calls), statusSeq(running(h)), testInterval, testTimeout, 3)
		cancel()
		if err != nil || !res.Success || atomic.LoadInt32(&calls) != 3 {
			t.Fatalf("health %q: res=%+v err=%v calls=%d", h, res, err, calls)
		}
	}
}

func TestGateHealthyThenUnhealthyMidGateFails(t *testing.T) {
	var calls int32
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st := statusSeq(running("healthy"), running("healthy"), running("unhealthy"))
	res, err := runHealthGate(ctx, countingProbe(true, &calls), st, testInterval, testTimeout, 5)
	if err != nil || res.Reason != HealthGateFailureContainerUnhealthy {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestClassifyContainerUnhealthyIsPermanentConfiguration(t *testing.T) {
	got := ClassifyHealthGateResult(HealthGateResult{Reason: HealthGateFailureContainerUnhealthy})
	if got != queue.FailureClassPermanentConfiguration {
		t.Fatalf("class = %v", got)
	}
}

func TestContainerRunningStateReadsHealthStatus(t *testing.T) {
	cases := map[string]string{
		`{"Id":"c1","State":{"Status":"running","Running":true,"Health":{"Status":"starting","FailingStreak":2,"Log":[]}}}`: "starting",
		`{"Id":"c1","State":{"Status":"running","Running":true,"Health":{"Status":"unhealthy"}}}`:                           "unhealthy",
		`{"Id":"c1","State":{"Status":"running","Running":true}}`:                                                           "",
	}
	for body, want := range cases {
		body := body
		c, err := newClient(startFakeEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(body))
		})))
		if err != nil {
			t.Fatal(err)
		}
		st, err := c.containerRunningState(context.Background(), "c1")
		if err != nil || st.State != ContainerStateRunning || st.Health != want {
			t.Fatalf("body %s: st=%+v err=%v want health %q", body, st, err, want)
		}
	}
}

func TestPolicyTimingsMapSecondsAndDefaults(t *testing.T) {
	d := policyTimings(healthgate.Policy{})
	if d.threshold != 3 || d.interval != 2*time.Second || d.probeTimeout != 2*time.Second || d.deadline != 60*time.Second {
		t.Fatalf("defaults = %+v", d)
	}
	d = policyTimings(healthgate.Policy{SuccessThreshold: 5, IntervalSeconds: 4, TimeoutSeconds: 3, DeadlineSeconds: 300})
	if d.threshold != 5 || d.interval != 4*time.Second || d.probeTimeout != 3*time.Second || d.deadline != 300*time.Second {
		t.Fatalf("tuned = %+v", d)
	}
}

func listenTCP(t *testing.T) (net.Listener, int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	t.Cleanup(func() { l.Close() })
	return l, l.Addr().(*net.TCPAddr).Port
}

func TestTCPProbeDialsAndTracesFailure(t *testing.T) {
	l, port := listenTCP(t)
	trace := &probeTrace{}
	p := newTCPProbe("127.0.0.1", port, trace)
	ok, err := p(context.Background())
	if err != nil || !ok {
		t.Fatalf("open port: ok=%v err=%v", ok, err)
	}
	l.Close()
	ok, err = p(context.Background())
	if err != nil || ok {
		t.Fatalf("closed port: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(trace.get(), "tcp") {
		t.Fatalf("trace = %q", trace.get())
	}
}

func TestBothProbesRequiredAndTCPFailureResetsStreak(t *testing.T) {
	var httpCalls, tcpCalls int32
	// tcp sequence: ok, ok, FAIL, ok, ok, ok -> needs all six rounds for threshold 3.
	seq := []bool{true, true, false, true, true, true}
	tcp := func(context.Context) (bool, error) {
		n := int(atomic.AddInt32(&tcpCalls, 1)) - 1
		return seq[n], nil
	}
	both := andProbes(countingProbe(true, &httpCalls), tcp)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := runHealthGate(ctx, both, alwaysRunning, testInterval, testTimeout, 3)
	if err != nil || !res.Success {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if atomic.LoadInt32(&tcpCalls) != 6 || atomic.LoadInt32(&httpCalls) != 6 {
		t.Fatalf("http=%d tcp=%d, want 6 each (dial every round)", httpCalls, tcpCalls)
	}
}

func TestBothProbesHTTPFailureStillFails(t *testing.T) {
	var tcpCalls int32
	both := andProbes(fixedProbe(false), countingProbe(true, &tcpCalls))
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	res, _ := runHealthGate(ctx, both, alwaysRunning, testInterval, testTimeout, 1)
	if res.Success {
		t.Fatal("tcp success must not mask an http failure")
	}
}
