package logship

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	agentclient "autodeploy/internal/agent/client"
)

type fakeSender struct {
	mu   sync.Mutex
	reqs []agentclient.AppendLogsRequest
	fn   func(call int, ctx context.Context, req agentclient.AppendLogsRequest) error
}

func (f *fakeSender) AppendLogs(ctx context.Context, req agentclient.AppendLogsRequest) (agentclient.AppendLogsResult, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	call := len(f.reqs)
	fn := f.fn
	f.mu.Unlock()
	if fn != nil {
		if err := fn(call, ctx, req); err != nil {
			return agentclient.AppendLogsResult{}, err
		}
	}
	return agentclient.AppendLogsResult{Accepted: len(req.Chunks)}, nil
}

func (f *fakeSender) requests() []agentclient.AppendLogsRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agentclient.AppendLogsRequest(nil), f.reqs...)
}

func (f *fakeSender) chunks(stream Stream) []agentclient.LogChunk {
	var out []agentclient.LogChunk
	for _, r := range f.requests() {
		for _, c := range r.Chunks {
			if c.Stream == string(stream) {
				out = append(out, c)
			}
		}
	}
	return out
}

func testFactory(t *testing.T, s Sender, mod func(*Config)) *Factory {
	t.Helper()
	cfg := Config{Sender: s, Logger: log.New(io.Discard, "", 0),
		BatchInterval: 10 * time.Millisecond, FlushBudget: 500 * time.Millisecond,
		CallTimeout: time.Second, RetryBase: time.Millisecond, RetryMax: 5 * time.Millisecond}
	if mod != nil {
		mod(&cfg)
	}
	f, err := NewFactory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func collect(a []agentclient.LogChunk) string {
	var sb strings.Builder
	for _, c := range a {
		sb.WriteString(c.Body)
		sb.WriteByte('\n')
	}
	return sb.String()
}

func TestAssemblerAnySplitSameLines(t *testing.T) {
	input := "alpha\nbeta\tgamma\x00delta\r\nüñí\xffcode\n" + strings.Repeat("é", 3000) + "\n" +
		strings.Repeat("x", 9000) + "\nlast-no-newline"
	run := func(split func(int) int) []string {
		var got []string
		a := newAssembler(func(b string) { got = append(got, b) })
		data := []byte(input)
		for len(data) > 0 {
			n := split(len(data))
			a.write(data[:n])
			data = data[n:]
		}
		a.flushFinal()
		return got
	}
	// Splitting a line with a newline mid-way does not change line content;
	// only the long-line split points may differ, so compare joined text per
	// logical line group by joining everything.
	whole := run(func(n int) int { return n })
	join := func(l []string) string { return strings.Join(l, "|") }
	for _, b := range whole {
		if len(b) > MaxChunkBytes || !utf8.ValidString(b) || strings.ContainsRune(b, 0) {
			t.Fatalf("bad chunk len=%d", len(b))
		}
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		got := run(func(n int) int { return 1 + rng.Intn(min(n, 700)) })
		// Chunk boundaries inside the over-long lines depend on the write
		// pattern; the concatenated text of all long-line pieces must match.
		if strings.ReplaceAll(join(got), "|", "") != strings.ReplaceAll(join(whole), "|", "") {
			t.Fatalf("split run %d changed content", i)
		}
		for _, b := range got {
			if len(b) > MaxChunkBytes || !utf8.ValidString(b) {
				t.Fatalf("bad chunk")
			}
		}
	}
	if whole[0] != "alpha" || whole[1] != "beta\tgammadelta" {
		t.Fatalf("unexpected lines %q", whole[:3])
	}
	if !strings.Contains(whole[2], "�") {
		t.Fatalf("invalid utf8 not replaced: %q", whole[2])
	}
}

func TestAssemblerSplitsOnRuneBoundary(t *testing.T) {
	var got []string
	a := newAssembler(func(b string) { got = append(got, b) })
	a.write([]byte(strings.Repeat("€", 5000))) // 3-byte runes
	a.flushFinal()
	if len(got) < 2 {
		t.Fatal("expected a split")
	}
	total := 0
	for _, b := range got {
		if !utf8.ValidString(b) || len(b) > MaxChunkBytes {
			t.Fatalf("bad chunk len=%d valid=%v", len(b), utf8.ValidString(b))
		}
		total += utf8.RuneCountInString(b)
	}
	if total != 5000 {
		t.Fatalf("lost runes: %d", total)
	}
}

const pem = "-----BEGIN KEY-----\nMIIsyntheticBODYline1\nMIIsyntheticBODYline2\n-----END KEY-----"

func syntheticSecrets() [][]byte {
	return [][]byte{
		[]byte("build-secret-AAAA1111"),
		[]byte("runtime-secret-BBBB2222"),
		[]byte("checkout-cred-ghs_CCCC3333"),
		[]byte(pem),
	}
}

func assertNoSecret(t *testing.T, f *fakeSender, secrets [][]byte) {
	t.Helper()
	for _, r := range f.requests() {
		for _, c := range r.Chunks {
			for _, s := range secrets {
				for _, form := range []string{string(s), base64.StdEncoding.EncodeToString(s), base64.RawStdEncoding.EncodeToString(s)} {
					if strings.Contains(c.Body, form) {
						t.Fatalf("secret form leaked in shipped chunk (stream %s)", c.Stream)
					}
				}
			}
		}
	}
	// Multi-line secret: no individual PEM line may survive either.
	for _, c := range f.chunks(StreamBuild) {
		if strings.Contains(c.Body, "syntheticBODYline") {
			t.Fatal("multi-line secret body line leaked")
		}
	}
}

func TestRedactionFuzzSplitsAndBoundaries(t *testing.T) {
	secrets := syntheticSecrets()
	rng := rand.New(rand.NewSource(7))
	for iter := 0; iter < 60; iter++ {
		fs := &fakeSender{}
		a := testFactory(t, fs, nil).Start(1, 2)
		a.SetSecrets(secrets)
		w := a.BuildWriter()
		var in bytes.Buffer
		for i := 0; i < 40; i++ {
			switch rng.Intn(5) {
			case 0:
				in.Write(secrets[rng.Intn(len(secrets))])
			case 1:
				in.WriteString(strings.Repeat("a", 3990+rng.Intn(20)))
				in.Write(secrets[rng.Intn(3)]) // straddles the 4000-byte split
			case 2:
				in.WriteString(base64.StdEncoding.EncodeToString(secrets[rng.Intn(len(secrets))]))
			case 3:
				in.WriteString("\n")
			default:
				in.WriteString(fmt.Sprintf("step %d: ok ", i))
			}
		}
		data := in.Bytes()
		for len(data) > 0 {
			n := 1 + rng.Intn(min(len(data), 300))
			if _, err := w.Write(data[:n]); err != nil {
				t.Fatal(err)
			}
			data = data[n:]
		}
		a.Close()
		assertNoSecret(t, fs, secrets)
		if !strings.Contains(collect(fs.chunks(StreamBuild)), "[REDACTED]") {
			t.Fatalf("iter %d: expected redaction marker", iter)
		}
	}
}

func TestRuntimeAndCheckoutSecretsRedactedBuildOutputOnly(t *testing.T) {
	// Runtime and checkout values are NOT in docker.Build's build-scoped set;
	// the shipper's pass is what masks them.
	fs := &fakeSender{}
	a := testFactory(t, fs, nil).Start(1, 1)
	a.SetSecrets(syntheticSecrets())
	w := a.BuildWriter()
	fmt.Fprintf(w, "env RUNTIME=runtime-secret-BBBB2222 tok=checkout-cred-ghs_CCCC3333\n")
	a.Linef(StreamPhase, "phase: x runtime-secret-BBBB2222")
	a.Close()
	out := collect(fs.chunks(StreamBuild)) + collect(fs.chunks(StreamPhase))
	if strings.Contains(out, "BBBB2222") || strings.Contains(out, "CCCC3333") {
		t.Fatalf("leak: %q", out)
	}
}

func TestBuildOutputSuppressedUntilSecretsKnown(t *testing.T) {
	fs := &fakeSender{}
	a := testFactory(t, fs, nil).Start(1, 1)
	w := a.BuildWriter()
	fmt.Fprintf(w, "token=zzz-not-yet-known\n")
	a.Linef(StreamPhase, "phase: building")
	a.Close()
	if strings.Contains(collect(fs.chunks(StreamBuild)), "zzz") {
		t.Fatal("build output shipped before the secret set was known")
	}
	if len(fs.chunks(StreamSystem)) != 1 || len(fs.chunks(StreamPhase)) != 1 {
		t.Fatalf("expected one system marker and one phase line, got %v", fs.requests())
	}
}

func TestRedactorFailureSuppressesFailClosed(t *testing.T) {
	fs := &fakeSender{}
	a := testFactory(t, fs, nil).Start(1, 1)
	a.SetSecrets([][]byte{[]byte("RED")}) // substring of the marker: redactor refuses
	fmt.Fprintf(a.BuildWriter(), "RED secret RED\n")
	a.Close()
	if len(fs.chunks(StreamBuild)) != 0 {
		t.Fatal("build output must be suppressed when the redactor cannot be built")
	}
}

func TestNULAndControlsStripped(t *testing.T) {
	fs := &fakeSender{}
	a := testFactory(t, fs, nil).Start(1, 1)
	a.SetSecrets(nil)
	fmt.Fprintf(a.BuildWriter(), "a\x00b\x01c\x1b[31mred\x1b[0m\td\x7f\n\x00\x00\n")
	a.Close()
	for _, c := range fs.chunks(StreamBuild) {
		for _, r := range c.Body {
			if r == 0 || (r < 0x20 && r != '\t') || r == 0x7f {
				t.Fatalf("control char in %q", c.Body)
			}
		}
	}
	if got := collect(fs.chunks(StreamBuild)); got != "abc[31mred[0m\td\n" {
		t.Fatalf("got %q", got)
	}
}

func TestOverflowDropNewestOneMarkerPerEpisodeAndBound(t *testing.T) {
	release := make(chan struct{})
	fs := &fakeSender{fn: func(call int, ctx context.Context, _ agentclient.AppendLogsRequest) error {
		if call == 1 {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}}
	const bufBytes = 4096
	a := testFactory(t, fs, func(c *Config) { c.BufferBytes = bufBytes; c.CallTimeout = 10 * time.Second }).Start(1, 1)
	a.SetSecrets(nil)
	w := a.BuildWriter()
	line := strings.Repeat("x", 100) + "\n"
	const total = 200
	for i := 0; i < total; i++ {
		w.Write([]byte(line))
		a.mu.Lock()
		if a.used > bufBytes {
			a.mu.Unlock()
			t.Fatalf("memory bound exceeded: %d", a.used)
		}
		a.mu.Unlock()
	}
	close(release)
	// Space has returned: further lines flow and the marker follows once.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(fs.chunks(StreamSystem)) == 0 {
		w.Write([]byte("after\n"))
		time.Sleep(15 * time.Millisecond)
	}
	a.Close()
	markers := fs.chunks(StreamSystem)
	if len(markers) != 1 {
		t.Fatalf("want exactly one marker, got %d", len(markers))
	}
	var n int
	if _, err := fmt.Sscanf(markers[0].Body, "[logs dropped: %d lines]", &n); err != nil || n <= 0 {
		t.Fatalf("bad marker %q", markers[0].Body)
	}
}

func TestBlockedServerNeverBlocksWriteAndFlushBudgetHonoured(t *testing.T) {
	block := make(chan struct{})
	fs := &fakeSender{fn: func(_ int, _ context.Context, _ agentclient.AppendLogsRequest) error {
		<-block // ignores ctx: worst case server
		return nil
	}}
	before := runtime.NumGoroutine()
	a := testFactory(t, fs, func(c *Config) { c.FlushBudget = 200 * time.Millisecond }).Start(1, 1)
	a.SetSecrets(nil)
	w := a.BuildWriter()
	start := time.Now()
	var worst time.Duration
	for i := 0; i < 20000; i++ {
		t0 := time.Now()
		w.Write([]byte("some build output line that keeps coming\n"))
		a.Linef(StreamPhase, "phase: %d", i)
		if d := time.Since(t0); d > worst {
			worst = d
		}
	}
	if time.Since(start) > 3*time.Second || worst > 250*time.Millisecond {
		t.Fatalf("Write blocked: total %v worst %v", time.Since(start), worst)
	}
	t0 := time.Now()
	a.Close()
	if d := time.Since(t0); d > 200*time.Millisecond+time.Second {
		t.Fatalf("Close exceeded the flush budget: %v", d)
	}
	close(block)
	waitGoroutines(t, before)
}

func waitGoroutines(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: before=%d now=%d", before, runtime.NumGoroutine())
}

func TestRetryableThenSuccessKeepsSeqs(t *testing.T) {
	fs := &fakeSender{fn: func(call int, _ context.Context, _ agentclient.AppendLogsRequest) error {
		if call <= 2 {
			return fmt.Errorf("%w: boom", agentclient.ErrTransportFailure)
		}
		return nil
	}}
	before := runtime.NumGoroutine()
	a := testFactory(t, fs, nil).Start(5, 6)
	a.SetSecrets(nil)
	w := a.BuildWriter()
	w.Write([]byte("one\ntwo\n"))
	a.Linef(StreamPhase, "phase: building")
	a.Close()
	reqs := fs.requests()
	if len(reqs) < 3 {
		t.Fatalf("expected retries, got %d requests", len(reqs))
	}
	for _, r := range reqs {
		if r.AttemptID != 5 || r.Fence != 6 || r.WriterEpoch != reqs[0].WriterEpoch {
			t.Fatalf("identity changed across retries: %+v", r)
		}
	}
	// The first batch (same chunk list, same seqs) is replayed verbatim.
	if fmt.Sprint(reqs[0].Chunks) != fmt.Sprint(reqs[1].Chunks) || fmt.Sprint(reqs[1].Chunks) != fmt.Sprint(reqs[2].Chunks) {
		t.Fatalf("retry resent different chunks: %v vs %v", reqs[0].Chunks, reqs[1].Chunks)
	}
	seen := map[string]int64{}
	for _, c := range reqs[len(reqs)-1].Chunks {
		if c.Seq <= seen[c.Stream] {
			t.Fatalf("seq not increasing")
		}
		seen[c.Stream] = c.Seq
	}
	waitGoroutines(t, before)
}

func TestSeqMonotonicPerStreamAndEpochUnique(t *testing.T) {
	fs := &fakeSender{}
	f := testFactory(t, fs, nil)
	a1 := f.Start(1, 1)
	a2 := f.Start(1, 1)
	if a1.epoch >= a2.epoch {
		t.Fatalf("epochs not strictly increasing: %d %d", a1.epoch, a2.epoch)
	}
	for i := 0; i < 50; i++ {
		a1.Linef(StreamPhase, "p %d", i)
		a1.Linef(StreamHealth, "h %d", i)
	}
	a1.Close()
	a2.Close()
	last := map[string]int64{}
	for _, r := range fs.requests() {
		if r.WriterEpoch != a1.epoch {
			continue
		}
		for _, c := range r.Chunks {
			if c.Seq != last[c.Stream]+1 {
				t.Fatalf("stream %s seq %d after %d", c.Stream, c.Seq, last[c.Stream])
			}
			last[c.Stream] = c.Seq
		}
	}
	if last["phase"] != 50 || last["health"] != 50 {
		t.Fatalf("missing chunks: %v", last)
	}
}

func TestPermanentErrorStopsShipping(t *testing.T) {
	fs := &fakeSender{fn: func(int, context.Context, agentclient.AppendLogsRequest) error {
		return agentclient.ErrUnauthorized
	}}
	var logged bytes.Buffer
	var lmu sync.Mutex
	before := runtime.NumGoroutine()
	a := testFactory(t, fs, func(c *Config) { c.Logger = log.New(lockedWriter{&lmu, &logged}, "", 0) }).Start(1, 1)
	a.Linef(StreamPhase, "phase: a")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(fs.requests()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	a.Linef(StreamPhase, "phase: b")
	a.Close()
	if n := len(fs.requests()); n != 1 {
		t.Fatalf("expected exactly one request after a permanent error, got %d", n)
	}
	lmu.Lock()
	defer lmu.Unlock()
	if strings.Count(logged.String(), "log shipping stopped") != 1 {
		t.Fatalf("expected one local log line, got %q", logged.String())
	}
	waitGoroutines(t, before)
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestCloseCancelsInFlightCallAndStopsCleanly(t *testing.T) {
	started := make(chan struct{})
	fs := &fakeSender{fn: func(_ int, ctx context.Context, _ agentclient.AppendLogsRequest) error {
		close(started)
		<-ctx.Done()
		return ctx.Err() // classified retryable; the shipper must stop anyway
	}}
	before := runtime.NumGoroutine()
	a := testFactory(t, fs, func(c *Config) { c.FlushBudget = 100 * time.Millisecond; c.CallTimeout = 10 * time.Second }).Start(1, 1)
	a.Linef(StreamPhase, "phase: a")
	<-started
	t0 := time.Now()
	a.Close()
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("Close took %v", d)
	}
	a.Close() // idempotent
	if n := len(fs.requests()); n != 1 {
		t.Fatalf("cancel must not retry: %d requests", n)
	}
	waitGoroutines(t, before)
}

func TestBatchLimits(t *testing.T) {
	fs := &fakeSender{}
	a := testFactory(t, fs, func(c *Config) { c.BufferBytes = 4 << 20 }).Start(1, 1)
	a.SetSecrets(nil)
	w := a.BuildWriter()
	for i := 0; i < 500; i++ {
		fmt.Fprintf(w, "<%s>\n", strings.Repeat("<&>", 1300)) // JSON-escapes 6x
	}
	a.Close()
	if len(fs.chunks(StreamBuild)) == 0 {
		t.Fatal("nothing shipped")
	}
	for _, r := range fs.requests() {
		if len(r.Chunks) > maxBatchChunks {
			t.Fatalf("batch has %d chunks", len(r.Chunks))
		}
		b, _ := jsonSize(r)
		if b > 64<<10 {
			t.Fatalf("request %d bytes exceeds server cap", b)
		}
	}
}

func TestNilAttemptAndFactoryAreNoOps(t *testing.T) {
	var f *Factory
	a := f.Start(1, 1)
	a.SetSecrets(nil)
	a.Linef(StreamPhase, "x")
	if n, err := a.BuildWriter().Write([]byte("x")); n != 1 || err != nil {
		t.Fatal("nil attempt writer")
	}
	a.Close()
	if FromContext(context.Background()) != nil {
		t.Fatal("expected nil")
	}
}

// jsonSize reports the encoded request size (test and bound-check helper).
func jsonSize(r agentclient.AppendLogsRequest) (int, error) {
	b, err := json.Marshal(r)
	return len(b), err
}

func TestInterleavedControlBytesCannotSplitSecretPastRedactor(t *testing.T) {
	secrets := syntheticSecrets()
	rng := rand.New(rand.NewSource(11))
	for iter := 0; iter < 40; iter++ {
		fs := &fakeSender{}
		a := testFactory(t, fs, nil).Start(1, 1)
		a.SetSecrets(secrets)
		w := a.BuildWriter()
		var in bytes.Buffer
		for i := 0; i < 10; i++ {
			for _, c := range secrets[rng.Intn(3)] {
				in.WriteByte(c)
				if rng.Intn(2) == 0 {
					in.WriteByte([]byte{0, 1, 0x1b, 0x7f, '\r'}[rng.Intn(5)])
				}
			}
			in.WriteString("\n")
		}
		data := in.Bytes()
		for len(data) > 0 {
			n := 1 + rng.Intn(min(len(data), 50))
			w.Write(data[:n])
			data = data[n:]
		}
		a.Close()
		assertNoSecret(t, fs, secrets)
	}
}
