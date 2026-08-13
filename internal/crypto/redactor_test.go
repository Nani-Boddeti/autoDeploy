package crypto

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestRedactingWriterEveryChunkBoundaryAndEncodings(t *testing.T) {
	for _, secret := range [][]byte{[]byte("hunter2"), {0, 1, 2, 3, 4}} {
		input := append(append([]byte("before "), secret...), []byte(" after")...)
		for split := 0; split <= len(input); split++ {
			var output bytes.Buffer
			writer, err := NewRedactingWriter(&output, [][]byte{secret})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(input[:split]); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(input[split:]); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(output.String(), string(secret)) || output.String() != "before [REDACTED] after" {
				t.Fatalf("split %d output %q", split, output.String())
			}
		}
	}
	secret := []byte("hunter2")
	for _, encoded := range [][]byte{secret, []byte(base64.StdEncoding.EncodeToString(secret)), []byte(base64.RawStdEncoding.EncodeToString(secret))} {
		for split := 0; split <= len(encoded); split++ {
			var output bytes.Buffer
			writer, err := NewRedactingWriter(&output, [][]byte{secret})
			if err != nil {
				t.Fatal(err)
			}
			_, _ = writer.Write(encoded[:split])
			_, _ = writer.Write(encoded[split:])
			if err := writer.Close(); err != nil || output.String() != Redacted {
				t.Fatalf("encoded split %d output=%q err=%v", split, output.String(), err)
			}
		}
	}
	var output bytes.Buffer
	writer, err := NewRedactingWriter(&output, [][]byte{[]byte("hunter2")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write([]byte("a aHVudGVyMg== aHVudGVyMg z"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if output.String() != "a [REDACTED] [REDACTED] z" {
		t.Fatalf("encoding output %q", output.String())
	}
}
func TestRedactingWriterFailsClosed(t *testing.T) {
	if _, err := NewRedactingWriter(nil, [][]byte{[]byte("secret")}); !errors.Is(err, ErrRedaction) {
		t.Fatalf("init %v", err)
	}
	if _, err := NewRedactingWriter(bytes.NewBuffer(nil), [][]byte{bytes.Repeat([]byte{'x'}, MaxRedactionRawBytes+1)}); !errors.Is(err, ErrRedaction) {
		t.Fatalf("bound %v", err)
	}
	failing := failingWriter{}
	writer, err := NewRedactingWriter(failing, [][]byte{[]byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("secret and enough data")); !errors.Is(err, ErrRedaction) {
		t.Fatalf("write %v", err)
	}
	if _, err := writer.Write([]byte("anything")); !errors.Is(err, ErrRedaction) {
		t.Fatalf("post failure %v", err)
	}
}

func TestRedactingWriterStreamingRegressions(t *testing.T) {
	t.Run("empty values pass through", func(t *testing.T) {
		var output bytes.Buffer
		writer, err := NewRedactingWriter(&output, [][]byte{nil, {}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte("public output")); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if got := output.String(); got != "public output" {
			t.Fatalf("output = %q", got)
		}
	})

	t.Run("overlaps use leftmost longest precedence", func(t *testing.T) {
		var output bytes.Buffer
		writer, err := NewRedactingWriter(&output, [][]byte{[]byte("abcd"), []byte("bc"), []byte("secret")})
		if err != nil {
			t.Fatal(err)
		}
		for _, chunk := range [][]byte{[]byte("xab"), []byte("cdsecret"), []byte("se"), []byte("cretz")} {
			if _, err := writer.Write(chunk); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if got, want := output.String(), "x[REDACTED][REDACTED][REDACTED]z"; got != want {
			t.Fatalf("output = %q, want %q", got, want)
		}
	})

	t.Run("one byte chunks redact long patterns", func(t *testing.T) {
		secret := bytes.Repeat([]byte("long-secret-"), 32)
		input := append(append([]byte("before "), secret...), []byte(" after")...)
		var output bytes.Buffer
		writer, err := NewRedactingWriter(&output, [][]byte{secret})
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range input {
			if _, err := writer.Write([]byte{b}); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if got, want := output.String(), "before [REDACTED] after"; got != want {
			t.Fatalf("output = %q, want %q", got, want)
		}
	})
}

func TestRedactingWriterFailsClosedForMarkerSynthesis(t *testing.T) {
	for _, test := range []struct {
		name   string
		values [][]byte
		input  string
	}{
		{"marker plus suffix", [][]byte{[]byte("secret"), []byte("[REDACTED]x")}, "secretx"},
		{"prefix plus marker", [][]byte{[]byte("secret"), []byte("x[RED")}, "xsecret"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			writer, err := NewRedactingWriter(&output, test.values)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write([]byte(test.input)); err != nil && !errors.Is(err, ErrRedaction) {
				t.Fatalf("write: %v", err)
			}
			if err := writer.Close(); !errors.Is(err, ErrRedaction) {
				t.Fatalf("close = %v", err)
			}
		})
	}
}

func TestNewRedactingWriterBounds(t *testing.T) {
	values := make([][]byte, MaxRedactionPatterns/3+1)
	for index := range values {
		values[index] = []byte{byte(index >> 8), byte(index)}
	}
	if _, err := NewRedactingWriter(bytes.NewBuffer(nil), values); !errors.Is(err, ErrRedaction) {
		t.Fatalf("pattern limit: %v", err)
	}
	if _, err := NewRedactingWriter(bytes.NewBuffer(nil), [][]byte{bytes.Repeat([]byte("x"), MaxRedactionNodes)}); !errors.Is(err, ErrRedaction) {
		t.Fatalf("node limit: %v", err)
	}
}

func TestRedactingWriterDoesNotRescanSourceAfterReplacement(t *testing.T) {
	short := []byte("q")
	long := bytes.Repeat(short, 20_000)
	input := append(append([]byte(nil), long...), bytes.Repeat(short, 7_000)...)
	var output bytes.Buffer
	writer, err := NewRedactingWriter(&output, [][]byte{short, long})
	if err != nil {
		t.Fatal(err)
	}
	for index, b := range input {
		if _, err := writer.Write([]byte{b}); err != nil {
			t.Fatalf("byte %d: %v (work %d, transitions %d)", index, err, writer.matchWork, writer.sourceTransitions)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), Redacted+strings.Repeat(Redacted, 7_000); got != want {
		t.Fatalf("unexpected output length %d, want %d", len(got), len(want))
	}
	if got, want := writer.sourceTransitions, uint64(len(input)); got != want {
		t.Fatalf("source transitions = %d, want one per input byte (%d)", got, want)
	}
	if limit := uint64(MaxRedactionPatterns) + writer.sourceTransitions*redactionMatchWorkPerByte; writer.matchWork > limit {
		t.Fatalf("match work = %d, limit %d", writer.matchWork, limit)
	}
	if got, want := len(writer.candidateLengths), writer.matcher.maxPattern; got != want {
		t.Fatalf("candidate ring = %d, want %d", got, want)
	}
}

func TestRedactingWriterFailsClosedOnNestedPatternAmplification(t *testing.T) {
	values := make([][]byte, MaxRedactionPatterns/3)
	for index := range values {
		values[index] = bytes.Repeat([]byte("q"), index+1)
	}
	var output bytes.Buffer
	writer, err := NewRedactingWriter(&output, values)
	if err != nil {
		t.Fatal(err)
	}
	input := bytes.Repeat([]byte("q"), len(values)*2)
	for _, b := range input {
		if _, err := writer.Write([]byte{b}); errors.Is(err, ErrRedaction) {
			return
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); !errors.Is(err, ErrRedaction) {
		t.Fatalf("nested patterns completed with work %d instead of failing closed: %v", writer.matchWork, err)
	}
}

func TestRedactingWriterMatchWorkLimitFailsClosedOnOverflow(t *testing.T) {
	writer := RedactingWriter{
		sourceTransitions: (maxRedactionWork-uint64(MaxRedactionPatterns))/redactionMatchWorkPerByte + 1,
	}
	if writer.consumeMatchWork() {
		t.Fatal("overflowing transition budget was accepted")
	}
	if writer.matchWork != 1 {
		t.Fatalf("work counter = %d, want 1", writer.matchWork)
	}
	writer = RedactingWriter{matchWork: maxRedactionWork}
	if writer.consumeMatchWork() || writer.matchWork != maxRedactionWork {
		t.Fatal("work counter overflow was not rejected without wrapping")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("downstream") }
