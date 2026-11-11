// Package logship ships an attempt's deploy-step log lines to the control
// plane (step 15g). Guarantees, in order of importance:
//
//   - Logging never fails, blocks or meaningfully slows a deployment. Writes
//     only touch memory under a short mutex; all network I/O happens on one
//     sender goroutine per attempt, and Close is bounded by a flush budget.
//   - Agent-side redaction is PRIMARY: every byte is passed through
//     crypto.RedactingWriter over the attempt's full materialized secret set
//     BEFORE it is line-assembled or buffered. Because redaction runs on the
//     raw byte stream, secrets split across Writes, across line-split
//     boundaries and multi-line secrets (e.g. PEM keys) are all covered.
//     Build output is suppressed (fail closed) until the secret set is known
//     (SetSecrets) or if a redactor cannot be built.
//   - Memory is bounded: a fixed byte budget for queued chunks, drop-newest on
//     overflow with one "[logs dropped: N lines]" system marker per overflow
//     episode.
//   - Replay safe: chunks get (stream, epoch, seq) at enqueue; a retried batch
//     resends the identical chunks, and the server de-duplicates on that key.
//     The epoch is unique per Start (monotonic), so a re-run of the same
//     attempt or a restarted agent can never collide with earlier seqs.
package logship

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/crypto"
)

// DroppedMarkerPrefix starts the system-stream line emitted once per overflow
// episode. The web log page matches it to style the marker.
const DroppedMarkerPrefix = "[logs dropped: "

// Stream names are the server's allowlist (migration 000022).
type Stream string

const (
	StreamBuild      Stream = "build"
	StreamPhase      Stream = "phase"
	StreamHealth     Stream = "health"
	StreamActivation Stream = "activation"
	StreamDrain      Stream = "drain"
	StreamSystem     Stream = "system"
)

func (s Stream) valid() bool {
	switch s {
	case StreamBuild, StreamPhase, StreamHealth, StreamActivation, StreamDrain, StreamSystem:
		return true
	}
	return false
}

const (
	defaultBufferBytes   = 256 << 10
	defaultBatchInterval = time.Second
	defaultFlushBudget   = 2 * time.Second
	defaultCallTimeout   = 5 * time.Second
	defaultRetryBase     = 250 * time.Millisecond
	defaultRetryMax      = 5 * time.Second
	defaultMaxBatchTries = 6

	maxBatchChunks   = 32
	maxRequestBytes  = 60 << 10 // server cap is 64 KiB
	requestOverhead  = 256
	wakeBatchBytes   = 32 << 10
	exitGraceAfterCx = 100 * time.Millisecond
)

// Sender is the control-plane capability the shipper needs;
// *agentclient.Client satisfies it.
type Sender interface {
	AppendLogs(ctx context.Context, req agentclient.AppendLogsRequest) (agentclient.AppendLogsResult, error)
}

// Config configures a Factory. Zero durations/sizes use the defaults.
type Config struct {
	Sender Sender
	Logger *log.Logger

	BufferBytes   int           // queued-chunk memory bound (default 256 KiB)
	BatchInterval time.Duration // time-based batching and idle partial-line flush (1s)
	FlushBudget   time.Duration // hard bound on Close (2s)
	CallTimeout   time.Duration // per AppendLogs call (5s)
	RetryBase     time.Duration // first retry delay, doubling (250ms)
	RetryMax      time.Duration // retry delay cap (5s)
	MaxBatchTries int           // attempts per batch before it is dropped (6)
}

func (c *Config) applyDefaults() {
	if c.Logger == nil {
		c.Logger = log.Default()
	}
	if c.BufferBytes <= 0 {
		c.BufferBytes = defaultBufferBytes
	}
	if c.BatchInterval <= 0 {
		c.BatchInterval = defaultBatchInterval
	}
	if c.FlushBudget <= 0 {
		c.FlushBudget = defaultFlushBudget
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = defaultCallTimeout
	}
	if c.RetryBase <= 0 {
		c.RetryBase = defaultRetryBase
	}
	if c.RetryMax <= 0 {
		c.RetryMax = defaultRetryMax
	}
	if c.MaxBatchTries <= 0 {
		c.MaxBatchTries = defaultMaxBatchTries
	}
}

// Factory starts one Attempt shipper per executor attempt. A nil *Factory is
// valid and starts nil (no-op) Attempts.
type Factory struct {
	cfg Config

	mu        sync.Mutex
	lastEpoch int64
}

// NewFactory validates cfg.
func NewFactory(cfg Config) (*Factory, error) {
	if cfg.Sender == nil {
		return nil, errors.New("logship: sender is required")
	}
	cfg.applyDefaults()
	return &Factory{cfg: cfg}, nil
}

// nextEpoch returns a strictly increasing, wall-clock-seeded epoch: unique per
// Start within the process and (barring a clock step back past a prior run)
// across restarts.
func (f *Factory) nextEpoch() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := time.Now().UnixNano()
	if e <= f.lastEpoch {
		e = f.lastEpoch + 1
	}
	f.lastEpoch = e
	return e
}

type queued struct {
	chunk agentclient.LogChunk
	size  int // JSON-encoded size, so request bounds are exact
}

// pipe is one raw byte stream (build stdout or stderr): redactor -> line
// assembler. Each BuildWriter call gets its own pipe so interleaved stdout and
// stderr chunks can never split one another's secrets.
type pipe struct {
	a        *Attempt
	red      *crypto.RedactingWriter
	asm      *assembler
	failed   bool
	warned   bool
	lastData time.Time
}

// Attempt ships the lines of one attempt. A nil *Attempt is valid: every
// method is a no-op and BuildWriter returns io.Discard.
type Attempt struct {
	cfg       Config
	sender    Sender
	attemptID int64
	fence     int64
	epoch     int64

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	done   chan struct{}

	mu         sync.Mutex
	secrets    [][]byte
	ready      bool
	pipes      []*pipe
	seq        map[Stream]int64
	queue      []queued
	inflight   []queued
	used       int
	droppedN   int
	closing    bool
	stopped    bool
	stopLogged bool
}

// Start begins shipping for one attempt. It never fails.
func (f *Factory) Start(attemptID, fence int64) *Attempt {
	if f == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &Attempt{
		cfg: f.cfg, sender: f.cfg.Sender,
		attemptID: attemptID, fence: fence, epoch: f.nextEpoch(),
		ctx: ctx, cancel: cancel,
		wake: make(chan struct{}, 1), done: make(chan struct{}),
		seq: make(map[Stream]int64),
	}
	go a.run()
	return a
}

type ctxKey struct{}

// WithAttempt carries a into ctx so deeper layers (activation) can write
// structural lines without signature changes.
func WithAttempt(ctx context.Context, a *Attempt) context.Context {
	return context.WithValue(ctx, ctxKey{}, a)
}

// FromContext returns the Attempt carried by ctx, or nil (a valid no-op).
func FromContext(ctx context.Context) *Attempt {
	a, _ := ctx.Value(ctxKey{}).(*Attempt)
	return a
}

// SetSecrets supplies the attempt's FULL materialized secret set (build- and
// runtime-scoped values, any credential). It must be called, even with nil
// when there is nothing secret, before build output can ship; until then (or
// if the set cannot be turned into a redactor) build writes are dropped. Later
// calls add to the set. The redaction rule is crypto.RedactingWriter's:
// exact bytes plus standard/raw base64 forms; no minimum length (a short
// value is masked everywhere it occurs, as in docker.Build).
func (a *Attempt) SetSecrets(values [][]byte) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// Later calls extend the set (used by Linef and new pipes); pipes whose
	// redactor already exists keep theirs.
	for _, v := range values {
		if len(v) > 0 {
			a.secrets = append(a.secrets, append([]byte(nil), v...))
		}
	}
	a.ready = true
}

// BuildWriter returns a new goroutine-safe writer for one raw build stream
// (call once for stdout and once for stderr). Write never blocks on the
// network and never returns an error.
func (a *Attempt) BuildWriter() io.Writer {
	if a == nil {
		return io.Discard
	}
	p := &pipe{a: a}
	p.asm = newAssembler(func(body string) { a.enqueueLocked(StreamBuild, body) })
	a.mu.Lock()
	a.pipes = append(a.pipes, p)
	a.mu.Unlock()
	return p
}

// Write implements io.Writer for a build pipe.
func (p *pipe) Write(b []byte) (int, error) {
	a := p.a
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(b)
	if a.closing || a.stopped || p.failed {
		return n, nil
	}
	if !a.ready {
		p.suppress("[build output not shipped: secret set unavailable]")
		return n, nil
	}
	if p.red == nil {
		red, err := crypto.NewRedactingWriter(sinkFunc(p.asm.write), a.secrets)
		if err != nil {
			p.suppress("[build output not shipped: redactor unavailable]")
			return n, nil
		}
		p.red = red
	}
	if _, err := p.red.Write(stripControls(b)); err != nil {
		p.suppress("[build output not shipped: redaction failure]")
		return n, nil
	}
	p.lastData = time.Now()
	a.wakeIfLargeLocked()
	return n, nil
}

// suppress permanently disables this pipe (fail closed) with one marker line.
func (p *pipe) suppress(marker string) {
	p.failed = true
	if p.red != nil {
		p.red = nil
	}
	p.asm.buf = p.asm.buf[:0]
	if !p.warned {
		p.warned = true
		p.a.enqueueLocked(StreamSystem, marker)
	}
}

type sinkFunc func([]byte)

func (f sinkFunc) Write(b []byte) (int, error) { f(b); return len(b), nil }

// Linef writes structural lines (phase markers, health summary, activation
// steps, drain outcome). Callers must pass only fixed formats with enum or
// numeric arguments, never error text, URLs or environment values; the text is
// still redacted over the secret set as defense in depth.
func (a *Attempt) Linef(stream Stream, format string, args ...any) {
	if a == nil {
		return
	}
	if !stream.valid() {
		stream = StreamSystem
	}
	text := []byte(fmt.Sprintf(format, args...) + "\n")
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing || a.stopped {
		return
	}
	if len(a.secrets) > 0 {
		var out bytes.Buffer
		red, err := crypto.NewRedactingWriter(&out, a.secrets)
		if err != nil {
			return
		}
		if _, err := red.Write(text); err != nil || red.Close() != nil {
			return
		}
		text = out.Bytes()
	}
	asm := newAssembler(func(body string) { a.enqueueLocked(stream, body) })
	asm.write(text)
	asm.flushFinal()
}

// enqueueLocked assigns (stream, seq) and queues body, or counts a drop when
// the byte budget is exhausted. a.mu must be held.
func (a *Attempt) enqueueLocked(stream Stream, body string) {
	if a.stopped {
		return
	}
	a.markerLocked(len(body))
	if !a.addLocked(stream, body) {
		a.droppedN++
	}
}

func (a *Attempt) addLocked(stream Stream, body string) bool {
	chunk := agentclient.LogChunk{Stream: string(stream), Seq: a.seq[stream] + 1, Body: body}
	enc, err := json.Marshal(chunk)
	if err != nil {
		return false
	}
	if a.used+len(enc) > a.cfg.BufferBytes {
		return false
	}
	a.seq[stream]++
	a.used += len(enc)
	a.queue = append(a.queue, queued{chunk: chunk, size: len(enc)})
	return true
}

// markerLocked emits the single "dropped" marker for the overflow episode once
// there is room for it plus extra bytes of the next line.
func (a *Attempt) markerLocked(extra int) {
	if a.droppedN == 0 {
		return
	}
	body := fmt.Sprintf(DroppedMarkerPrefix+"%d lines]", a.droppedN)
	if a.used+len(body)+64+extra+64 > a.cfg.BufferBytes {
		return
	}
	if a.addLocked(StreamSystem, body) {
		a.droppedN = 0
	}
}

func (a *Attempt) wakeIfLargeLocked() {
	if len(a.queue) >= maxBatchChunks || a.used >= wakeBatchBytes {
		a.signal()
	}
}

func (a *Attempt) signal() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// Close flushes remaining lines within the flush budget and stops the
// shipper. It is idempotent, never returns an error and never blocks longer
// than the budget (plus a short grace for goroutine exit).
func (a *Attempt) Close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if !a.closing {
		a.closing = true
		if !a.stopped {
			for _, p := range a.pipes {
				if p.red != nil && !p.failed {
					_ = p.red.Close()
				}
				p.asm.flushFinal()
			}
			a.markerLocked(0)
		}
	}
	a.mu.Unlock()
	a.signal()

	timer := time.NewTimer(a.cfg.FlushBudget)
	defer timer.Stop()
	select {
	case <-a.done:
	case <-timer.C:
	}
	a.cancel()
	grace := time.NewTimer(exitGraceAfterCx)
	defer grace.Stop()
	select {
	case <-a.done:
	case <-grace.C:
	}
}

func (a *Attempt) run() {
	defer close(a.done)
	ticker := time.NewTicker(a.cfg.BatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			a.idleFlush()
		case <-a.wake:
		case <-a.ctx.Done():
			return
		}
		if !a.drain() {
			if a.ctx.Err() != nil {
				return
			}
		}
		a.mu.Lock()
		finished := a.closing && (a.stopped || (len(a.queue) == 0 && len(a.inflight) == 0))
		a.mu.Unlock()
		if finished {
			return
		}
	}
}

// idleFlush ships partial lines that have sat idle for a batch interval.
func (a *Attempt) idleFlush() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped || a.closing {
		return
	}
	now := time.Now()
	for _, p := range a.pipes {
		if p.asm.pending() && now.Sub(p.lastData) >= a.cfg.BatchInterval {
			p.asm.flushIdle()
		}
	}
	a.markerLocked(0)
}

// drain sends batches until the queue is empty. It returns false when
// shipping must stop (permanent error or cancelled ctx).
func (a *Attempt) drain() bool {
	for {
		batch := a.takeBatch()
		if len(batch) == 0 {
			return true
		}
		ok := a.sendWithRetry(batch)
		a.mu.Lock()
		if a.stopped {
			a.mu.Unlock()
			return false
		}
		if ok {
			a.completeLocked(batch)
		}
		a.mu.Unlock()
		if !ok {
			return false
		}
	}
}

func (a *Attempt) takeBatch() []queued {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return nil
	}
	a.markerLocked(0)
	n, bytesUsed := 0, requestOverhead
	for n < len(a.queue) && n < maxBatchChunks {
		sz := a.queue[n].size + 1
		if n > 0 && bytesUsed+sz > maxRequestBytes {
			break
		}
		bytesUsed += sz
		n++
	}
	if n == 0 {
		return nil
	}
	batch := append([]queued(nil), a.queue[:n]...)
	rest := copy(a.queue, a.queue[n:])
	for i := rest; i < len(a.queue); i++ {
		a.queue[i] = queued{}
	}
	a.queue = a.queue[:rest]
	a.inflight = batch
	return batch
}

// completeLocked releases a finished (sent or abandoned) batch's bytes.
func (a *Attempt) completeLocked(batch []queued) {
	for _, q := range batch {
		a.used -= q.size
	}
	a.inflight = nil
}

// sendWithRetry returns true when the batch is done with (sent, or abandoned
// after MaxBatchTries and counted as dropped), false when shipping stops.
func (a *Attempt) sendWithRetry(batch []queued) bool {
	req := agentclient.AppendLogsRequest{AttemptID: a.attemptID, Fence: a.fence, WriterEpoch: a.epoch,
		Chunks: make([]agentclient.LogChunk, len(batch))}
	for i, q := range batch {
		req.Chunks[i] = q.chunk
	}
	delay := a.cfg.RetryBase
	for try := 1; ; try++ {
		if a.ctx.Err() != nil {
			return false
		}
		callCtx, cancel := context.WithTimeout(a.ctx, a.cfg.CallTimeout)
		_, err := a.sender.AppendLogs(callCtx, req)
		cancel()
		// ClassifyLogError maps a cancelled ctx to retryable; our own ctx is
		// authoritative for "stop".
		if a.ctx.Err() != nil && err != nil {
			return false
		}
		switch agentclient.ClassifyLogError(err) {
		case agentclient.LogSuccess:
			return true
		case agentclient.LogPermanent:
			a.stop(err)
			return false
		}
		if try >= a.cfg.MaxBatchTries {
			a.mu.Lock()
			a.droppedN += len(batch)
			a.mu.Unlock()
			return true
		}
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-a.ctx.Done():
			t.Stop()
			return false
		}
		if delay *= 2; delay > a.cfg.RetryMax {
			delay = a.cfg.RetryMax
		}
	}
}

// stop ends shipping for this attempt after a permanent refusal: the buffer is
// dropped and later writes are discarded. Logged locally once.
func (a *Attempt) stop(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopped = true
	a.queue, a.inflight, a.used = nil, nil, 0
	if !a.stopLogged {
		a.stopLogged = true
		a.cfg.Logger.Printf("agent logship: attempt %d: log shipping stopped: %v", a.attemptID, err)
	}
}

// stripControls removes NUL, C0 (except tab and newline) and DEL BEFORE
// redaction, so interleaved control bytes cannot split a secret past the
// matcher. The post-assembly sanitize still runs.
func stripControls(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if (c < 0x20 && c != '\t' && c != '\n') || c == 0x7f {
			continue
		}
		out = append(out, c)
	}
	return out
}
