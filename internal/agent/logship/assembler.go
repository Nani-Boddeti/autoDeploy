package logship

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

// MaxChunkBytes is the largest body the shipper emits. The server accepts
// 4096; 4000 leaves headroom and keeps every chunk on a rune boundary.
const MaxChunkBytes = 4000

// assembler turns arbitrary byte writes into sanitized, bounded text lines.
// It is not goroutine-safe; the owning Attempt serializes access. emit gets
// non-blank bodies that are valid UTF-8, free of NUL/C0 controls (except
// tab) and at most MaxChunkBytes long.
//
// Raw bytes are buffered, never more than MaxChunkBytes (+ at most 3 bytes of
// an incomplete rune), so memory per assembler is constant regardless of how
// long a line is.
type assembler struct {
	buf  []byte
	emit func(body string)
}

func newAssembler(emit func(body string)) *assembler {
	return &assembler{emit: emit}
}

func (a *assembler) write(p []byte) {
	for len(p) > 0 {
		room := MaxChunkBytes - len(a.buf)
		window := p
		if len(window) > room {
			window = p[:room]
		}
		if i := bytes.IndexByte(window, '\n'); i >= 0 {
			a.buf = append(a.buf, window[:i]...)
			p = p[i+1:]
			a.emitLine(a.buf)
			a.buf = a.buf[:0]
			continue
		}
		a.buf = append(a.buf, window...)
		p = p[len(window):]
		if len(a.buf) >= MaxChunkBytes {
			a.splitFull()
		}
	}
}

// splitFull emits a full buffer that has no newline, keeping a trailing
// incomplete rune (if any) for the next write.
func (a *assembler) splitFull() {
	cut := len(a.buf)
	if tail := incompleteTail(a.buf); tail > 0 {
		cut -= tail
	}
	a.emitLine(a.buf[:cut])
	n := copy(a.buf, a.buf[cut:])
	a.buf = a.buf[:n]
}

// flushIdle emits the buffered partial line (idle flush), holding back a
// trailing incomplete rune. flushFinal emits everything.
func (a *assembler) flushIdle() { a.flush(true) }

func (a *assembler) flushFinal() { a.flush(false) }

func (a *assembler) flush(holdTail bool) {
	if len(a.buf) == 0 {
		return
	}
	cut := len(a.buf)
	if holdTail {
		cut -= incompleteTail(a.buf)
	}
	if cut == 0 {
		return
	}
	a.emitLine(a.buf[:cut])
	n := copy(a.buf, a.buf[cut:])
	a.buf = a.buf[:n]
}

func (a *assembler) pending() bool { return len(a.buf) > 0 }

// incompleteTail returns the length of a trailing, possibly valid but still
// incomplete UTF-8 sequence (0..3 bytes).
func incompleteTail(b []byte) int {
	for i := 1; i <= 3 && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if !utf8.FullRune(b[len(b)-i:]) {
				return i
			}
			return 0
		}
	}
	return 0
}

// emitLine sanitizes raw and emits it in pieces of at most MaxChunkBytes
// (sanitizing can grow the text: an invalid byte becomes a 3-byte U+FFFD).
func (a *assembler) emitLine(raw []byte) {
	s := sanitize(raw)
	for len(s) > MaxChunkBytes {
		cut := MaxChunkBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		a.emitBody(s[:cut])
		s = s[cut:]
	}
	a.emitBody(s)
}

func (a *assembler) emitBody(s string) {
	if strings.TrimSpace(s) == "" {
		return
	}
	a.emit(s)
}

// sanitize replaces invalid UTF-8 with U+FFFD and strips NUL and the other C0
// controls except tab, plus DEL. (The store rejects a whole batch on NUL.)
func sanitize(raw []byte) string {
	s := strings.ToValidUTF8(string(raw), "�")
	return strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\t') || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
