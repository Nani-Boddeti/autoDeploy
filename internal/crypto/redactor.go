package crypto

import (
	"encoding/base64"
	"errors"
	"io"
	"sort"
)

const (
	Redacted                    = "[REDACTED]"
	MaxRedactionPatterns        = 4096
	MaxRedactionRawBytes        = 8 << 20
	MaxRedactionDerivedPatterns = MaxRedactionPatterns
	// MaxRedactionNodes bounds the compact automata independently of raw input.
	// Very long secrets remain valid input but fail closed when their derived trie
	// would consume more than this bounded amount of process memory.
	MaxRedactionNodes = 1 << 16
	redactionChunk    = 32 << 10
	// Bound nested-pattern output-link work independently of input size.
	redactionMatchWorkPerByte = 8
	maxRedactionWork          = ^uint64(0)
)

var ErrRedaction = errors.New("secret redaction failed")

type acEdge struct {
	byte byte
	node int
}

type acNode struct {
	edges      []acEdge
	fail       int
	outputLink int // nearest failure ancestor with a terminal pattern
	terminal   int // longest pattern ending exactly at this node
	output     int // longest pattern ending at this node or any failure ancestor
}

// redactionMatcher is a compact, immutable Aho-Corasick automaton. Edges are
// sorted slices rather than one heap map per node.
type redactionMatcher struct {
	nodes      []acNode
	maxPattern int
}

func (matcher redactionMatcher) advance(state int, b byte) int {
	for state != 0 {
		if next, ok := matcher.edge(state, b); ok {
			return next
		}
		state = matcher.nodes[state].fail
	}
	if next, ok := matcher.edge(0, b); ok {
		return next
	}
	return 0
}

func (matcher redactionMatcher) edge(state int, b byte) (int, bool) {
	edges := matcher.nodes[state].edges
	index := sort.Search(len(edges), func(index int) bool { return edges[index].byte >= b })
	if index < len(edges) && edges[index].byte == b {
		return edges[index].node, true
	}
	return 0, false
}

func newMatcher(patterns [][]byte) (redactionMatcher, error) {
	matcher := redactionMatcher{nodes: []acNode{{}}}
	for _, pattern := range patterns {
		state := 0
		if len(pattern) > matcher.maxPattern {
			matcher.maxPattern = len(pattern)
		}
		for _, b := range pattern {
			next, ok := matcher.edge(state, b)
			if !ok {
				if len(matcher.nodes) >= MaxRedactionNodes {
					return redactionMatcher{}, ErrRedaction
				}
				next = len(matcher.nodes)
				matcher.nodes = append(matcher.nodes, acNode{})
				edges := matcher.nodes[state].edges
				index := sort.Search(len(edges), func(index int) bool { return edges[index].byte >= b })
				edges = append(edges, acEdge{})
				copy(edges[index+1:], edges[index:])
				edges[index] = acEdge{byte: b, node: next}
				matcher.nodes[state].edges = edges
			}
			state = next
		}
		if len(pattern) > matcher.nodes[state].terminal {
			matcher.nodes[state].terminal = len(pattern)
			matcher.nodes[state].output = len(pattern)
		}
	}

	queue := make([]int, 0, len(matcher.nodes))
	for _, edge := range matcher.nodes[0].edges {
		queue = append(queue, edge.node)
	}
	for head := 0; head < len(queue); head++ {
		state := queue[head]
		for _, edge := range matcher.nodes[state].edges {
			failure := matcher.nodes[state].fail
			for failure != 0 {
				if next, ok := matcher.edge(failure, edge.byte); ok {
					failure = next
					break
				}
				failure = matcher.nodes[failure].fail
			}
			if failure == 0 {
				if next, ok := matcher.edge(0, edge.byte); ok && next != edge.node {
					failure = next
				}
			}
			matcher.nodes[edge.node].fail = failure
			if matcher.nodes[failure].terminal > 0 {
				matcher.nodes[edge.node].outputLink = failure
			} else {
				matcher.nodes[edge.node].outputLink = matcher.nodes[failure].outputLink
			}
			if matcher.nodes[failure].output > matcher.nodes[edge.node].output {
				matcher.nodes[edge.node].output = matcher.nodes[failure].output
			}
			queue = append(queue, edge.node)
		}
	}
	return matcher, nil
}

// RedactingWriter transforms secret-bearing output with stateful streaming
// matchers. Close flushes bounded source and output holdbacks.
type RedactingWriter struct {
	dst io.Writer

	matcher           redactionMatcher
	sourceState       int
	sourceOffset      int64
	sourceTransitions uint64
	pending           []byte
	pendingBase       int64
	candidateStarts   []int64
	candidateLengths  []int
	matchWork         uint64

	// safety scans bytes after substitution. Holding the maximum pattern suffix
	// prevents the fixed marker and surrounding output from synthesizing a secret.
	safety      redactionMatcher
	safetyState int
	safePending []byte
	failed      bool
}

func NewRedactingWriter(dst io.Writer, values [][]byte) (*RedactingWriter, error) {
	if dst == nil {
		return nil, ErrRedaction
	}
	patterns, err := redactionPatterns(values)
	if err != nil {
		return nil, ErrRedaction
	}
	matcher, err := newMatcher(patterns)
	if err != nil {
		return nil, ErrRedaction
	}
	return &RedactingWriter{
		dst:              dst,
		matcher:          matcher,
		safety:           matcher,
		candidateStarts:  make([]int64, matcher.maxPattern),
		candidateLengths: make([]int, matcher.maxPattern),
	}, nil
}

func redactionPatterns(values [][]byte) ([][]byte, error) {
	unique := make(map[string]struct{})
	patterns := make([][]byte, 0, len(values)*3)
	total := 0
	for _, value := range values {
		if len(value) == 0 {
			continue
		}
		if len(value) > MaxRedactionRawBytes-total {
			return nil, ErrRedaction
		}
		total += len(value)
		for _, pattern := range [][]byte{value, []byte(base64.StdEncoding.EncodeToString(value)), []byte(base64.RawStdEncoding.EncodeToString(value))} {
			key := string(pattern)
			if _, exists := unique[key]; exists {
				continue
			}
			if len(patterns) >= MaxRedactionPatterns {
				return nil, ErrRedaction
			}
			if containsMarkerConflict(pattern) {
				return nil, ErrRedaction
			}
			unique[key] = struct{}{}
			patterns = append(patterns, append([]byte(nil), pattern...))
		}
	}
	return patterns, nil
}

func containsMarkerConflict(pattern []byte) bool {
	return len(pattern) > 0 && indexBytes([]byte(Redacted), pattern) >= 0
}

func indexBytes(haystack, needle []byte) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		equal := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				equal = false
				break
			}
		}
		if equal {
			return i
		}
	}
	return -1
}

func (writer *RedactingWriter) Write(input []byte) (int, error) {
	if writer.failed {
		return 0, ErrRedaction
	}
	if writer.matcher.maxPattern == 0 {
		if err := writeAll(writer.dst, input); err != nil {
			writer.failed = true
			return 0, ErrRedaction
		}
		return len(input), nil
	}
	accepted := len(input)
	for len(input) > 0 {
		n := len(input)
		if n > redactionChunk {
			n = redactionChunk
		}
		if err := writer.accept(input[:n]); err != nil {
			writer.failed = true
			return 0, ErrRedaction
		}
		input = input[n:]
	}
	return accepted, nil
}

func (writer *RedactingWriter) accept(input []byte) error {
	for _, b := range input {
		writer.pending = append(writer.pending, b)
		writer.sourceState = writer.matcher.advance(writer.sourceState, b)
		writer.sourceOffset++
		writer.sourceTransitions++
		writer.recordSourceMatches()
		if writer.failed {
			return ErrRedaction
		}
		if err := writer.resolveSource(false); err != nil {
			return err
		}
	}
	return nil
}

func (writer *RedactingWriter) recordSourceMatches() {
	for state := writer.sourceState; state != 0; state = writer.matcher.nodes[state].outputLink {
		if !writer.consumeMatchWork() {
			writer.failed = true
			return
		}
		if length := writer.matcher.nodes[state].terminal; length > 0 {
			writer.recordMatch(writer.sourceOffset-int64(length), length)
		}
	}
}

func (writer *RedactingWriter) recordMatch(start int64, length int) {
	if start < writer.pendingBase {
		return
	}
	if start >= writer.pendingBase+int64(len(writer.candidateLengths)) {
		writer.failed = true
		return
	}
	if !writer.consumeMatchWork() {
		writer.failed = true
		return
	}
	index := int(start % int64(len(writer.candidateLengths)))
	if writer.candidateStarts[index] != start || length > writer.candidateLengths[index] {
		writer.candidateStarts[index], writer.candidateLengths[index] = start, length
	}
}

func (writer *RedactingWriter) consumeMatchWork() bool {
	if writer.matchWork == maxRedactionWork {
		return false
	}
	writer.matchWork++
	limit, ok := redactionMatchWorkLimit(writer.sourceTransitions)
	return ok && writer.matchWork <= limit
}

func redactionMatchWorkLimit(transitions uint64) (uint64, bool) {
	if transitions > (maxRedactionWork-uint64(MaxRedactionPatterns))/redactionMatchWorkPerByte {
		return 0, false
	}
	return uint64(MaxRedactionPatterns) + transitions*redactionMatchWorkPerByte, true
}

// resolveSource emits only bytes that cannot start a later match. A candidate
// waits for one maximum-pattern window, so overlapping matches use leftmost,
// then longest precedence without exposing an early replacement marker.
func (writer *RedactingWriter) resolveSource(final bool) error {
	for {
		if len(writer.pending) == 0 {
			return nil
		}
		if !final && writer.sourceOffset < writer.pendingBase+int64(writer.matcher.maxPattern) {
			return nil
		}
		if !writer.consumeMatchWork() {
			return ErrRedaction
		}
		index := int(writer.pendingBase % int64(len(writer.candidateLengths)))
		length := 0
		if writer.candidateStarts[index] == writer.pendingBase {
			length = writer.candidateLengths[index]
		}
		writer.candidateStarts[index], writer.candidateLengths[index] = 0, 0
		if length == 0 {
			length = 1
			if err := writer.emitSafe(writer.pending[:1], false); err != nil {
				return err
			}
		} else if length > len(writer.pending) {
			return ErrRedaction
		} else if err := writer.emitSafe([]byte(Redacted), false); err != nil {
			return err
		}
		for offset := 1; offset < length; offset++ {
			if !writer.consumeMatchWork() {
				return ErrRedaction
			}
			candidateIndex := int((writer.pendingBase + int64(offset)) % int64(len(writer.candidateLengths)))
			writer.candidateStarts[candidateIndex], writer.candidateLengths[candidateIndex] = 0, 0
		}
		clear(writer.pending[:length])
		writer.pending = writer.pending[length:]
		writer.pendingBase += int64(length)
	}
}

func (writer *RedactingWriter) emitSafe(input []byte, final bool) error {
	if writer.safety.maxPattern == 0 {
		return writeAll(writer.dst, input)
	}
	for _, b := range input {
		writer.safePending = append(writer.safePending, b)
		writer.safetyState = writer.safety.advance(writer.safetyState, b)
		if writer.safety.nodes[writer.safetyState].output > 0 {
			return ErrRedaction
		}
	}
	hold := writer.safety.maxPattern - 1
	flush := len(writer.safePending) - hold
	if final {
		flush = len(writer.safePending)
	}
	if flush <= 0 {
		return nil
	}
	if err := writeAll(writer.dst, writer.safePending[:flush]); err != nil {
		return err
	}
	copy(writer.safePending, writer.safePending[flush:])
	remaining := len(writer.safePending) - flush
	clear(writer.safePending[remaining:])
	writer.safePending = writer.safePending[:remaining]
	return nil
}

func (writer *RedactingWriter) Close() error {
	if writer.failed {
		return ErrRedaction
	}
	if writer.matcher.maxPattern != 0 {
		if err := writer.resolveSource(true); err != nil {
			writer.failed = true
			return ErrRedaction
		}
		if err := writer.emitSafe(nil, true); err != nil {
			writer.failed = true
			return ErrRedaction
		}
	}
	return nil
}

func writeAll(dst io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := dst.Write(data)
		if n < 0 || n > len(data) {
			return ErrRedaction
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
