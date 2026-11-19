// Package metrics is a minimal Prometheus text exposition (format 0.0.4)
// registry: counters and function-backed gauges only, no dependencies.
//
// Cardinality rule: label NAMES are restricted to a fixed allow-list enforced
// at registration, so owner, project, environment, host or repository names
// can never become label dimensions.
package metrics

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// ContentType is the exposition format's media type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

var allowedLabels = map[string]bool{
	"surface": true, "code": true, "result": true, "state": true, "server_id": true, "version": true,
}

// LabelAllowed reports whether name may be used as a label name.
func LabelAllowed(name string) bool { return allowedLabels[name] }

// Sample is one labelled value; LabelValues align with the declared names.
type Sample struct {
	LabelValues []string
	Value       float64
}

// entry is one metric family ready to render.
type entry struct {
	name, help, typ string
	labels          []string
	samples         []Sample
}

// family produces its entries once per WriteText.
type family interface {
	collect() []entry
}

// Registry holds registered metric families. It is safe for concurrent use.
type Registry struct {
	mu       sync.Mutex
	families []family
	descs    map[string]Descriptor
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{descs: map[string]Descriptor{}} }

func validName(s string, colon bool) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (colon && c == ':') || (i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// register panics on an invalid name, a disallowed or malformed label name, or
// a duplicate: all are programming errors caught at process start.
func (r *Registry) register(f family, descs ...Descriptor) {
	for _, d := range descs {
		if !validName(d.Name, true) {
			panic(fmt.Sprintf("metrics: invalid metric name %q", d.Name))
		}
		for _, l := range d.Labels {
			if !validName(l, false) || strings.HasPrefix(l, "__") || !LabelAllowed(l) {
				panic(fmt.Sprintf("metrics: label %q is not allowed on %s", l, d.Name))
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range descs {
		if _, dup := r.descs[d.Name]; dup {
			panic(fmt.Sprintf("metrics: duplicate metric %s", d.Name))
		}
	}
	for _, d := range descs {
		d.Labels = slices.Clone(d.Labels)
		r.descs[d.Name] = d
	}
	r.families = append(r.families, f)
}

// Descriptor is the registration view of one metric, for tests that audit the
// registry.
type Descriptor struct {
	Name   string
	Labels []string
}

// Descriptors returns every registered metric's name and label names.
func (r *Registry) Descriptors() []Descriptor {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Descriptor, 0, len(r.descs))
	for _, d := range r.descs {
		out = append(out, Descriptor{Name: d.Name, Labels: slices.Clone(d.Labels)})
	}
	slices.SortFunc(out, func(a, b Descriptor) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// WriteText writes every non-empty family in exposition format, ordered by
// metric name then label values. A family with no samples is omitted.
func (r *Registry) WriteText(w io.Writer) error {
	r.mu.Lock()
	fams := slices.Clone(r.families)
	r.mu.Unlock()
	var entries []entry
	for _, f := range fams {
		entries = append(entries, f.collect()...)
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.name, b.name) })
	var b bytes.Buffer
	for _, e := range entries {
		writeFamily(&b, e.name, e.help, e.typ, e.labels, e.samples)
	}
	_, err := w.Write(b.Bytes())
	return err
}

func writeFamily(b *bytes.Buffer, name, help, typ string, labels []string, samples []Sample) {
	if len(samples) == 0 {
		return
	}
	slices.SortFunc(samples, func(x, y Sample) int { return slices.Compare(x.LabelValues, y.LabelValues) })
	b.WriteString("# HELP " + name + " " + escapeHelp(help) + "\n# TYPE " + name + " " + typ + "\n")
	for _, s := range samples {
		b.WriteString(name)
		if len(labels) > 0 {
			b.WriteByte('{')
			for i, l := range labels {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(l + `="` + escapeLabel(s.LabelValues[i]) + `"`)
			}
			b.WriteByte('}')
		}
		b.WriteByte(' ')
		b.WriteString(formatFloat(s.Value))
		b.WriteByte('\n')
	}
}

var (
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
)

func escapeHelp(s string) string  { return helpEscaper.Replace(s) }
func escapeLabel(s string) string { return labelEscaper.Replace(s) }

func formatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// validSamples drops samples whose label arity does not match.
func validSamples(samples []Sample, labels []string) []Sample {
	out := samples[:0:0]
	for _, s := range samples {
		if len(s.LabelValues) == len(labels) {
			out = append(out, s)
		}
	}
	return out
}

type gaugeFunc struct {
	metric, help string
	labels       []string
	fn           func() []Sample
}

func (g *gaugeFunc) collect() []entry {
	return []entry{{g.metric, g.help, "gauge", g.labels, validSamples(g.fn(), g.labels)}}
}

// NewGaugeFunc registers a gauge collected by calling fn on every scrape. A
// returned sample whose label arity does not match is dropped.
func (r *Registry) NewGaugeFunc(name, help string, labels []string, fn func() []Sample) {
	g := &gaugeFunc{metric: name, help: help, labels: slices.Clone(labels), fn: fn}
	r.register(g, Descriptor{Name: name, Labels: g.labels})
}

// GaugeDesc declares one gauge of a GaugeSet.
type GaugeDesc struct {
	Name, Help string
	Labels     []string
}

type gaugeSet struct {
	descs []GaugeDesc
	fetch func() map[string][]Sample
}

func (g *gaugeSet) collect() []entry {
	got := g.fetch()
	var out []entry
	for _, d := range g.descs {
		out = append(out, entry{d.Name, d.Help, "gauge", d.Labels, validSamples(got[d.Name], d.Labels)})
	}
	return out
}

// NewGaugeSet registers several gauges fed by ONE collect call per
// WriteText, keyed by metric name, so related gauges (for example every
// gauge derived from one database snapshot) always agree within a scrape.
func (r *Registry) NewGaugeSet(descs []GaugeDesc, collect func() map[string][]Sample) {
	g := &gaugeSet{fetch: collect}
	var ds []Descriptor
	for _, d := range descs {
		d.Labels = slices.Clone(d.Labels)
		g.descs = append(g.descs, d)
		ds = append(ds, Descriptor{Name: d.Name, Labels: d.Labels})
	}
	r.register(g, ds...)
}

// Counter is a monotonically increasing value per label-value combination.
type Counter struct {
	metric string
	help   string
	labels []string
	mu     sync.Mutex
	series map[string]*series
}

type series struct {
	values []string
	n      uint64
	mu     sync.Mutex
}

// NewCounter registers a counter with a fixed set of label names.
func (r *Registry) NewCounter(name, help string, labels ...string) *Counter {
	c := &Counter{metric: name, help: help, labels: slices.Clone(labels), series: map[string]*series{}}
	r.register(c, Descriptor{Name: name, Labels: c.labels})
	return c
}

func (c *Counter) get(values []string) *series {
	if len(values) != len(c.labels) {
		panic(fmt.Sprintf("metrics: %s takes %d label values, got %d", c.metric, len(c.labels), len(values)))
	}
	key := strings.Join(values, "\xff")
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.series[key]
	if !ok {
		s = &series{values: slices.Clone(values)}
		c.series[key] = s
	}
	return s
}

// Touch creates the series at zero so it is exported before its first event.
func (c *Counter) Touch(labelValues ...string) { c.get(labelValues) }

// Inc adds one.
func (c *Counter) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Add adds n to the series.
func (c *Counter) Add(n uint64, labelValues ...string) {
	s := c.get(labelValues)
	s.mu.Lock()
	s.n += n
	s.mu.Unlock()
}

func (c *Counter) collect() []entry {
	c.mu.Lock()
	samples := make([]Sample, 0, len(c.series))
	for _, s := range c.series {
		s.mu.Lock()
		samples = append(samples, Sample{LabelValues: s.values, Value: float64(s.n)})
		s.mu.Unlock()
	}
	c.mu.Unlock()
	return []entry{{c.metric, c.help, "counter", c.labels, samples}}
}
