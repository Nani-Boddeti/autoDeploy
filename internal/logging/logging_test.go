package logging

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const secret = "postgres://user:hunter2pass@db:5432/x"

func newTest(t *testing.T, values ...string) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var vs [][]byte
	for _, v := range values {
		vs = append(vs, []byte(v))
	}
	var buf bytes.Buffer
	l, err := NewControlPlane(&buf, vs)
	if err != nil {
		t.Fatal(err)
	}
	return l, &buf
}

func TestRedactsExactAndBase64(t *testing.T) {
	l, buf := newTest(t, secret)
	l.Info("boom "+secret, "b64", base64.StdEncoding.EncodeToString([]byte(secret)), "raw", base64.RawStdEncoding.EncodeToString([]byte(secret)))
	out := buf.String()
	if strings.Contains(out, "hunter2pass") || strings.Contains(out, base64.StdEncoding.EncodeToString([]byte(secret))) || strings.Contains(out, base64.RawStdEncoding.EncodeToString([]byte(secret))) {
		t.Fatalf("leak: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("no marker: %q", out)
	}
}

func TestNoHoldbackCompleteLineAfterOneCall(t *testing.T) {
	l, buf := newTest(t, secret)
	l.Info("hello", "stage", "x")
	out := buf.String()
	if !strings.HasSuffix(out, "\n") || !strings.Contains(out, "stage=x") {
		t.Fatalf("line held back or incomplete: %q", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("want exactly one line: %q", out)
	}
}

func TestRegexPass(t *testing.T) {
	l, buf := newTest(t)
	l.Warn("x Bearer abcdefgh12345 ghs_" + strings.Repeat("a", 20) + " srv.v1." + strings.Repeat("b", 20))
	out := buf.String()
	for _, bad := range []string{"abcdefgh12345", strings.Repeat("a", 20), strings.Repeat("b", 20)} {
		if strings.Contains(out, bad) {
			t.Fatalf("leak %q in %q", bad, out)
		}
	}
}

func TestNoTimeAndLevels(t *testing.T) {
	l, buf := newTest(t)
	if l.Enabled(context.Background(), slog.LevelDebug) || !l.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("level gate wrong")
	}
	l.Debug("d")
	l.Error("e", KeyComponent, "c")
	out := buf.String()
	if strings.Contains(out, "time=") || strings.Contains(out, "msg=d") {
		t.Fatalf("unexpected: %q", out)
	}
	if out != "level=ERROR msg=e component=c\n" {
		t.Fatalf("got %q", out)
	}
}

func TestWithAttrsAndGroupRedacted(t *testing.T) {
	l, buf := newTest(t, secret)
	l.With(KeyComponent, "agent", "dsn", secret).WithGroup("g").Info("m", "k", "v", "s", secret)
	out := buf.String()
	for _, want := range []string{"component=agent", "dsn=[REDACTED]", "g.k=v", "g.s=[REDACTED]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "hunter2pass") {
		t.Fatalf("leak %q", out)
	}
}

func TestSecretSplitAcrossAttrsNotLeaked(t *testing.T) {
	// The record is redacted as one line, so a secret assembled in the message
	// is caught even when also present in an attribute.
	l, buf := newTest(t, "supersecretvalue")
	l.Info("a supersecret"+"value b", "x", "supersecretvalue")
	if strings.Contains(buf.String(), "supersecretvalue") {
		t.Fatalf("leak %q", buf.String())
	}
}

func TestConcurrentWholeLines(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	l, err := NewAgent(&lockedWriter{&mu, &buf}, [][]byte{[]byte(secret)})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				l.With("g", "w").Info("line", "v", secret, "n", j)
			}
		}()
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 1000 {
		t.Fatalf("lines=%d", len(lines))
	}
	for _, ln := range lines {
		if !strings.HasPrefix(ln, "level=INFO msg=line g=w v=[REDACTED] n=") {
			t.Fatalf("interleaved or leaked: %q", ln)
		}
	}
}

type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func TestRedactionFailureWritesFixedLine(t *testing.T) {
	var buf bytes.Buffer
	h, err := newHandler(&buf, [][]byte{[]byte(secret)})
	if err != nil {
		t.Fatal(err)
	}
	h.core.redact = func(*core, []byte) ([]byte, error) { return nil, errors.New("boom") }
	slog.New(h).Info("leaks " + secret)
	if buf.String() != droppedLine || strings.Contains(buf.String(), "hunter2pass") {
		t.Fatalf("got %q", buf.String())
	}
}

func TestConstructionValidatesValues(t *testing.T) {
	// A value that is a substring of the redaction marker is rejected.
	if _, err := NewControlPlane(&bytes.Buffer{}, [][]byte{[]byte("REDACT")}); err == nil {
		t.Fatal("want error")
	}
	if _, err := NewAgent(nil, nil); err == nil {
		t.Fatal("want error for nil dst")
	}
}

func TestErrAttr(t *testing.T) {
	if a := Err(errors.New("x")); a.Key != "error" || a.Value.String() != "x" {
		t.Fatalf("%v", a)
	}
}

// The text handler quotes and escapes values containing a space, '=', '"',
// control or non-printable bytes, so a secret with such bytes is emitted in
// escaped form. Exact-value redaction must cover that form too.
var escapedSecrets = map[string]string{
	"quote":     `pa"ss-word-1`,
	"backslash": `pa\ss-word-2`,
	"newline":   "pass\nword-3",
	"tab":       "pass\tword-4",
	"cr":        "pass\rword-5",
	"nonprint":  "pass\x01\x7fword-6",
	"invalid":   "pass\xffword-7",
	"unicode":   "pass word-8",
	"space-eq":  "pass word=9",
}

func TestEscapedSecretFormsRedacted(t *testing.T) {
	for name, sec := range escapedSecrets {
		t.Run(name, func(t *testing.T) {
			forms := []string{sec, strings.Trim(strconv.Quote(sec), `"`)}
			for _, mode := range []string{"message", "attr", "both"} {
				l, buf := newTest(t, sec)
				switch mode {
				case "message":
					l.Info("before " + sec + " after")
				case "attr":
					l.Info("m", "k", sec)
				default:
					l.With("w", sec).Info("before "+sec, "k", sec)
				}
				out := buf.String()
				for _, f := range forms {
					if strings.Contains(out, f) {
						t.Fatalf("%s: form %q leaked in %q", mode, f, out)
					}
				}
				if !strings.Contains(out, "[REDACTED]") || strings.Count(out, "\n") != 1 {
					t.Fatalf("%s: bad output %q", mode, out)
				}
			}
		})
	}
}

func TestEscapedSecretBase64StillRedacted(t *testing.T) {
	sec := `pa"ss-word-1`
	l, buf := newTest(t, sec)
	l.Info("m", "k", base64.StdEncoding.EncodeToString([]byte(sec)))
	if strings.Contains(buf.String(), base64.StdEncoding.EncodeToString([]byte(sec))) {
		t.Fatalf("leak %q", buf.String())
	}
}

// escapeForText must agree with what slog's text handler emits.
func TestEscapedFormMatchesTextHandler(t *testing.T) {
	for name, sec := range escapedSecrets {
		var b bytes.Buffer
		slog.New(slog.NewTextHandler(&b, nil)).Info("x "+sec, "k", sec)
		esc := escapeForText(sec)
		if esc == sec {
			continue
		}
		if !strings.Contains(b.String(), esc) {
			t.Errorf("%s: handler output %q lacks escaped form %q", name, b.String(), esc)
		}
	}
}
