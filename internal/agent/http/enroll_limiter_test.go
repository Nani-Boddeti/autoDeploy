package agenthttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"autodeploy/internal/store/postgres"
)

type fakeNow struct{ t time.Time }

func (f *fakeNow) now() time.Time { return f.t }

func TestEnrollLimiterBurstAndRefill(t *testing.T) {
	clk := &fakeNow{t: time.Unix(1000, 0)}
	l := NewEnrollLimiter(10, 10, 8, clk.now)
	for i := 0; i < 10; i++ {
		if ok, _ := l.Allow("srv_a"); !ok {
			t.Fatalf("attempt %d refused within burst", i)
		}
	}
	ok, wait := l.Allow("srv_a")
	if ok || wait <= 0 || wait > 7*time.Second {
		t.Fatalf("11th attempt: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.Allow("srv_b"); !ok {
		t.Fatal("other id must have its own bucket")
	}
	clk.t = clk.t.Add(6 * time.Second)
	if ok, _ := l.Allow("srv_a"); !ok {
		t.Fatal("token not refilled after 6s")
	}
	if ok, _ := l.Allow("srv_a"); ok {
		t.Fatal("only one token should have refilled")
	}
}

func TestEnrollLimiterMapIsBounded(t *testing.T) {
	l := NewEnrollLimiter(10, 10, 16, nil)
	for i := 0; i < 1000; i++ {
		l.Allow("srv_" + strconv.Itoa(i))
		if l.Len() > 16 {
			t.Fatalf("limiter grew to %d entries", l.Len())
		}
	}
	if l.Len() != 16 || len(l.byID) != 16 {
		t.Fatalf("len=%d map=%d", l.Len(), len(l.byID))
	}
}

func TestEnrollLimiterConcurrent(t *testing.T) {
	l := NewEnrollLimiter(10, 10, 32, nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				if ok, _ := l.Allow("srv_one"); ok {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if allowed < 10 || allowed > 11 {
		t.Fatalf("allowed=%d, want about 10", allowed)
	}
}

func enrollBody(t *testing.T, id string) string {
	t.Helper()
	enrTok, _ := testEnrollmentToken(t)
	return `{"server_id":"` + id + `","enrollment_token":"` + string(enrTok) + `","credential_digest":"` + hexDigest([32]byte{1}) + `"}`
}

// denyUnless fails enrolls with ErrServerAuthorizationDenied unless the
// handler is told the token is good.
type toggleEnroller struct {
	good bool
	n    int
}

func (e *toggleEnroller) Enroll(context.Context, string, [32]byte, [32]byte) (int, error) {
	e.n++
	if e.good {
		return 1, nil
	}
	return 0, postgres.ErrServerAuthorizationDenied
}

func TestHandleEnrollFailuresChargedThen429(t *testing.T) {
	fe := &toggleEnroller{}
	h := Handler{Enroller: fe, EnrollLimiter: NewEnrollLimiter(2, 2, 8, (&fakeNow{t: time.Unix(5, 0)}).now)}
	body := enrollBody(t, "server_1")
	var codes []int
	var last *httptest.ResponseRecorder
	for i := 0; i < 3; i++ {
		last = do(h, newRequestTLS(newRequest(t, pathEnroll, body, "", "")))
		codes = append(codes, last.Code)
	}
	if codes[0] != http.StatusUnauthorized || codes[1] != http.StatusUnauthorized || codes[2] != http.StatusTooManyRequests || last.Header().Get("Retry-After") == "" {
		t.Fatalf("codes=%v retry=%q", codes, last.Header().Get("Retry-After"))
	}
}

func TestHandleEnrollValidTokenSucceedsAfterAttackerExhaustsBucket(t *testing.T) {
	fe := &toggleEnroller{}
	lim := NewEnrollLimiter(2, 2, 8, (&fakeNow{t: time.Unix(5, 0)}).now)
	h := Handler{Enroller: fe, EnrollLimiter: lim}
	body := enrollBody(t, "server_x")
	for i := 0; i < 5; i++ {
		do(h, newRequestTLS(newRequest(t, pathEnroll, body, "", "")))
	}
	if w := do(h, newRequestTLS(newRequest(t, pathEnroll, body, "", ""))); w.Code != http.StatusTooManyRequests {
		t.Fatalf("attacker not throttled: %d", w.Code)
	}
	fe.good = true
	for i := 0; i < 3; i++ {
		if w := do(h, newRequestTLS(newRequest(t, pathEnroll, body, "", ""))); w.Code != http.StatusOK {
			t.Fatalf("valid enroll %d refused: %d", i, w.Code)
		}
	}
}

func TestHandleEnrollSuccessCreatesNoBucketAndMapStaysBounded(t *testing.T) {
	fe := &toggleEnroller{good: true}
	lim := NewEnrollLimiter(2, 2, 4, nil)
	h := Handler{Enroller: fe, EnrollLimiter: lim}
	for i := 0; i < 20; i++ {
		do(h, newRequestTLS(newRequest(t, pathEnroll, enrollBody(t, "server_"+strconv.Itoa(i)), "", "")))
	}
	if lim.Len() != 0 {
		t.Fatalf("successes created %d buckets", lim.Len())
	}
	fe.good = false
	for i := 0; i < 50; i++ {
		do(h, newRequestTLS(newRequest(t, pathEnroll, enrollBody(t, "server_"+strconv.Itoa(i)), "", "")))
		if lim.Len() > 4 {
			t.Fatalf("limiter grew to %d", lim.Len())
		}
	}
}
