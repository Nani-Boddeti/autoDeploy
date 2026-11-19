// Package logging builds the process loggers for both binaries on log/slog:
// key=value text, levels INFO/WARN/ERROR, stable attribute keys, and every
// record redacted before it reaches the destination.
//
// Redaction is per record, not streaming. crypto.RedactingWriter holds back
// maxPattern-1 bytes until the next Write or Close; wrapping a long-lived one
// under slog would delay the tail of every line until the next log line. So
// each record is formatted into a buffer, run through a fresh
// RedactingWriter (exact values and their base64 forms, fully flushed), then
// through the regex second pass (internal/logsanitize), and written to the
// destination in a single Write under a mutex. If redaction fails, a fixed
// line is written instead; the unredacted record is never emitted.
//
// Deadlock warning: Handle holds the mutex across formatting and the write.
// A slog.LogValuer or fmt.Stringer attribute, or a destination writer, must
// never log through the same logger.
//
// Control-plane rule: log a fixed message plus stage and class
// (errclass.Of(err)) attributes only. Do NOT use Err there: error text can
// carry configuration values. Err exists for agent code, where the whole
// record is redacted against the agent credential and the regex patterns.
package logging

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	secretcrypto "autodeploy/internal/crypto"
	"autodeploy/internal/logsanitize"
)

// Stable attribute keys.
const (
	KeyComponent    = "component"
	KeyAttemptID    = "attempt_id"
	KeyDeploymentID = "deployment_id"
	KeyReleaseID    = "release_id"
	KeyCleanupID    = "cleanup_id"
	KeyFailureClass = "failure_class"
	KeyStage        = "stage"
	KeyClass        = "class"
	KeyError        = "error"
)

// droppedLine is written when redaction fails.
const droppedLine = "level=ERROR msg=\"log record dropped: redaction failed\"\n"

// Err returns the error attribute. Agent code only; see the package doc.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.String(KeyError, "<nil>")
	}
	return slog.String(KeyError, err.Error())
}

// NewControlPlane returns a logger redacting values (and the regex patterns).
func NewControlPlane(dst io.Writer, values [][]byte) (*slog.Logger, error) {
	return newLogger(dst, values)
}

// NewAgent is NewControlPlane for the agent; values will be the credential.
func NewAgent(dst io.Writer, values [][]byte) (*slog.Logger, error) {
	return newLogger(dst, values)
}

func newLogger(dst io.Writer, values [][]byte) (*slog.Logger, error) {
	h, err := newHandler(dst, values)
	if err != nil {
		return nil, err
	}
	return slog.New(h), nil
}

// core is shared by a handler and everything derived via WithAttrs/WithGroup:
// one mutex, one scratch buffer, one destination.
type core struct {
	mu     sync.Mutex
	dst    io.Writer
	buf    bytes.Buffer
	values [][]byte
	// redact is a test seam; the default is redactLine.
	redact func(core *core, line []byte) ([]byte, error)
}

type handler struct {
	core  *core
	inner slog.Handler
}

func newHandler(dst io.Writer, values [][]byte) (*handler, error) {
	// Validate the values (with their escaped forms, which are what the text
	// handler emits for values needing quotes) once so a bad configuration
	// surfaces at construction.
	values = withEscapedForms(values)
	probe, err := secretcrypto.NewRedactingWriter(io.Discard, values)
	if err != nil {
		return nil, err
	}
	_ = probe.Close()
	if dst == nil {
		return nil, secretcrypto.ErrRedaction
	}
	c := &core{dst: dst, values: cloneValues(values), redact: redactLine}
	return &handler{core: c, inner: newInner(c)}, nil
}

func newInner(c *core) slog.Handler {
	return slog.NewTextHandler(&c.buf, &slog.HandlerOptions{
		Level: slog.LevelInfo,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
}

func cloneValues(values [][]byte) [][]byte {
	out := make([][]byte, len(values))
	for i, v := range values {
		out[i] = append([]byte(nil), v...)
	}
	return out
}

func redactLine(c *core, line []byte) ([]byte, error) {
	var out bytes.Buffer
	w, err := secretcrypto.NewRedactingWriter(&out, c.values)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(line); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return []byte(logsanitize.Redact(out.String())), nil
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelInfo }

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	c := h.core
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Reset()
	if err := h.inner.Handle(ctx, r); err != nil {
		c.buf.Reset()
		_, werr := io.WriteString(c.dst, droppedLine)
		return werr
	}
	line, err := c.redact(c, c.buf.Bytes())
	c.buf.Reset()
	if err != nil || len(line) == 0 || line[len(line)-1] != '\n' {
		_, werr := io.WriteString(c.dst, droppedLine)
		return werr
	}
	_, werr := c.dst.Write(line)
	return werr
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &handler{core: h.core, inner: h.inner.WithAttrs(attrs)}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{core: h.core, inner: h.inner.WithGroup(name)}
}

// escapeForText returns v as the text handler emits it inside a quoted value
// or message (the form a secret takes when it contains a space, '=', '"', a
// backslash next to quoted text, or control, non-printable or invalid-UTF-8
// bytes). It is derived by formatting through slog.NewTextHandler itself, with
// a trailing space forcing the quoted form, so it cannot drift from slog's
// own escaping. It returns v unchanged when there is nothing to escape.
func escapeForText(v string) string {
	var b bytes.Buffer
	h := slog.NewTextHandler(&b, &slog.HandlerOptions{ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && (a.Key == slog.TimeKey || a.Key == slog.LevelKey) {
			return slog.Attr{}
		}
		return a
	}})
	r := slog.NewRecord(time.Time{}, slog.LevelInfo, v+" ", 0)
	if err := h.Handle(context.Background(), r); err != nil {
		return v
	}
	out := b.String()
	const prefix, suffix = `msg="`, " \"\n"
	if !strings.HasPrefix(out, prefix) || !strings.HasSuffix(out, suffix) {
		return v
	}
	return out[len(prefix) : len(out)-len(suffix)]
}

// withEscapedForms returns values plus, for each value whose text-handler
// escaped form differs, that escaped form. The extra forms go through the same
// RedactingWriter validation (size, pattern-count and marker-conflict limits).
func withEscapedForms(values [][]byte) [][]byte {
	out := make([][]byte, 0, len(values)*2)
	for _, v := range values {
		out = append(out, v)
		if esc := escapeForText(string(v)); esc != string(v) {
			out = append(out, []byte(esc))
		}
	}
	return out
}
