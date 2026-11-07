// Package promtexttest is a strict parser for the Prometheus text exposition
// format, for use in tests only: it rejects everything the format disallows
// and several things it tolerates but a scraper would be right to distrust
// (see Parse). Non-test code must not import it.
package promtexttest

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

type Family struct {
	Name, Help, Type string
	Samples          []Sample
}

type Exposition struct{ Families []Family }

// Family returns the named family, or nil if absent.
func (e *Exposition) Family(name string) *Family {
	for i := range e.Families {
		if e.Families[i].Name == name {
			return &e.Families[i]
		}
	}
	return nil
}

// Find returns the first sample, whatever its name, whose labels contain every
// given name/value pair; it may carry additional labels. For a histogram
// family iterate Samples to pick a series by name.
func (f *Family) Find(labels map[string]string) (Sample, bool) {
	for _, s := range f.Samples {
		match := true
		for k, v := range labels {
			if got, ok := s.Labels[k]; !ok || got != v {
				match = false
				break
			}
		}
		if match {
			return s, true
		}
	}
	return Sample{}, false
}

// Value is Find with labels given as alternating name, value pairs.
func (f *Family) Value(labels ...string) (float64, bool) {
	if len(labels)%2 != 0 {
		panic("promtexttest: Value needs name, value pairs")
	}
	m := make(map[string]string, len(labels)/2)
	for i := 0; i < len(labels); i += 2 {
		m[labels[i]] = labels[i+1]
	}
	s, ok := f.Find(m)
	return s.Value, ok
}

// Parse reads a text exposition and fails on anything the format does not
// allow or that a scraper would be right to distrust:
//
//   - the output must be UTF-8, end with a newline and contain no blank line;
//   - every family has exactly one HELP then one TYPE, both before its first
//     sample, and its samples are contiguous; a sample of a family without
//     both headers is an error;
//   - TYPE is counter, gauge, histogram, summary or untyped, and counter names
//     end in _total;
//   - metric and label names are valid, label names do not start with "__"
//     or repeat, label values are quoted with only the \\, \" and \n escapes;
//   - no series (name plus label set) appears twice;
//   - values are Go floats or +Inf, -Inf, NaN, optionally followed by an
//     integer timestamp, with single spaces as separators;
//   - for each label set of a histogram family the _bucket series have
//     strictly ascending le ending in +Inf, non-decreasing counts, and a _sum
//     and a _count equal to the +Inf bucket;
//   - comment lines other than HELP and TYPE are rejected.
func Parse(r io.Reader) (*Exposition, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("promtexttest: invalid UTF-8")
	}
	p := &parser{idx: map[string]int{}, cur: -1, series: map[string]struct{}{}}
	if len(data) > 0 {
		if data[len(data)-1] != '\n' {
			return nil, fmt.Errorf("promtexttest: output does not end with a newline")
		}
		for i, line := range strings.Split(string(data[:len(data)-1]), "\n") {
			if err := p.parseLine(line); err != nil {
				return nil, fmt.Errorf("promtexttest: line %d: %w", i+1, err)
			}
		}
	}
	for i := range p.fs {
		if p.fs[i].Type == "histogram" {
			if err := checkHistogram(&p.fs[i].Family); err != nil {
				return nil, fmt.Errorf("promtexttest: %w", err)
			}
		}
	}
	e := &Exposition{Families: make([]Family, len(p.fs))}
	for i := range p.fs {
		e.Families[i] = p.fs[i].Family
	}
	return e, nil
}

type famState struct {
	Family
	hasHelp, hasType bool
}

type parser struct {
	fs     []famState
	idx    map[string]int // family name -> index in fs
	cur    int            // family of the most recent header or sample, -1 at start
	series map[string]struct{}
}

func (p *parser) parseLine(line string) error {
	switch {
	case line == "":
		return fmt.Errorf("blank line")
	case strings.HasSuffix(line, "\r"):
		return fmt.Errorf("carriage return before the newline")
	case strings.HasPrefix(line, "# HELP "):
		return p.parseHelp(line[len("# HELP "):])
	case strings.HasPrefix(line, "# TYPE "):
		return p.parseType(line[len("# TYPE "):])
	case line[0] == '#':
		return fmt.Errorf("comment line %q is neither HELP nor TYPE", line)
	}
	return p.parseSample(line)
}

// header returns the family a HELP or TYPE line is about, creating it on first
// mention and enforcing that headers precede samples and families stay contiguous.
func (p *parser) header(name, kind string) (*famState, error) {
	if !validMetricName(name) {
		return nil, fmt.Errorf("invalid metric name %q", name)
	}
	i, ok := p.idx[name]
	if !ok {
		p.fs = append(p.fs, famState{Family: Family{Name: name}})
		i = len(p.fs) - 1
		p.idx[name] = i
		p.cur = i
		return &p.fs[i], nil
	}
	f := &p.fs[i]
	switch {
	case i != p.cur:
		return nil, fmt.Errorf("%s for family %s after another family started", kind, name)
	case len(f.Samples) > 0:
		return nil, fmt.Errorf("%s for family %s after its first sample", kind, name)
	}
	return f, nil
}

func (p *parser) parseHelp(rest string) error {
	name, text, _ := strings.Cut(rest, " ")
	f, err := p.header(name, "HELP")
	if err != nil {
		return err
	}
	if f.hasHelp {
		return fmt.Errorf("duplicate HELP for %s", name)
	}
	help, err := unescape(text)
	if err != nil {
		return fmt.Errorf("HELP for %s: %w", name, err)
	}
	f.hasHelp, f.Help = true, help
	return nil
}

func (p *parser) parseType(rest string) error {
	name, typ, ok := strings.Cut(rest, " ")
	if !ok {
		return fmt.Errorf("TYPE line without a type")
	}
	f, err := p.header(name, "TYPE")
	if err != nil {
		return err
	}
	switch typ {
	case "counter", "gauge", "histogram", "summary", "untyped":
	default:
		return fmt.Errorf("unknown type %q for %s", typ, name)
	}
	switch {
	case f.hasType:
		return fmt.Errorf("duplicate TYPE for %s", name)
	case !f.hasHelp:
		return fmt.Errorf("TYPE for %s without a preceding HELP", name)
	case typ == "counter" && !strings.HasSuffix(name, "_total"):
		return fmt.Errorf("counter %s does not end in _total", name)
	}
	f.hasType, f.Type = true, typ
	return nil
}

func (f *famState) accepts(name string) bool {
	switch f.Type {
	case "histogram":
		return isSuffixOf(name, f.Name, "_bucket", "_sum", "_count")
	case "summary":
		return name == f.Name || isSuffixOf(name, f.Name, "_sum", "_count")
	}
	return name == f.Name
}

func isSuffixOf(name, base string, suffixes ...string) bool {
	if !strings.HasPrefix(name, base) {
		return false
	}
	for _, s := range suffixes {
		if name[len(base):] == s {
			return true
		}
	}
	return false
}

// familyFor finds the declared family a sample name belongs to.
func (p *parser) familyFor(name string) (int, bool) {
	if p.cur >= 0 && p.fs[p.cur].accepts(name) {
		return p.cur, true
	}
	if i, ok := p.idx[name]; ok && p.fs[i].accepts(name) {
		return i, true
	}
	for _, sfx := range []string{"_bucket", "_sum", "_count"} {
		if base, ok := strings.CutSuffix(name, sfx); ok {
			if i, ok := p.idx[base]; ok && p.fs[i].accepts(name) {
				return i, true
			}
		}
	}
	return -1, false
}

func (p *parser) parseSample(line string) error {
	i := strings.IndexAny(line, "{ ")
	if i < 0 {
		return fmt.Errorf("sample %q has no value", line)
	}
	name := line[:i]
	if !validMetricName(name) {
		return fmt.Errorf("invalid metric name %q", name)
	}
	labels := map[string]string{}
	if line[i] == '{' {
		var err error
		if i, err = parseLabels(line, i+1, labels); err != nil {
			return fmt.Errorf("sample %s: %w", name, err)
		}
	}
	if i >= len(line) || line[i] != ' ' {
		return fmt.Errorf("sample %s: expected a single space before the value", name)
	}
	valStr, ts, hasTS := strings.Cut(line[i+1:], " ")
	if hasTS && !validTimestamp(ts) {
		return fmt.Errorf("sample %s: invalid timestamp %q", name, ts)
	}
	value, err := parseValue(valStr)
	if err != nil {
		return fmt.Errorf("sample %s: %w", name, err)
	}

	fi, ok := p.familyFor(name)
	if !ok {
		return fmt.Errorf("sample %s has no HELP/TYPE header in a family that accepts it", name)
	}
	f := &p.fs[fi]
	switch {
	case fi != p.cur:
		return fmt.Errorf("sample %s of family %s after another family started", name, f.Name)
	case !f.hasType:
		return fmt.Errorf("sample %s before the TYPE of %s", name, f.Name)
	}
	key := seriesKey(name, labels)
	if _, dup := p.series[key]; dup {
		return fmt.Errorf("duplicate series %s", key)
	}
	p.series[key] = struct{}{}
	f.Samples = append(f.Samples, Sample{Name: name, Labels: labels, Value: value})
	return nil
}

// parseLabels parses label pairs after the opening brace at line[i-1] and
// returns the index just past the closing brace.
func parseLabels(line string, i int, labels map[string]string) (int, error) {
	for {
		if i >= len(line) {
			return 0, fmt.Errorf("unterminated label set")
		}
		if line[i] == '}' {
			return i + 1, nil
		}
		eq := strings.IndexByte(line[i:], '=')
		if eq < 0 {
			return 0, fmt.Errorf("label without =")
		}
		ln := line[i : i+eq]
		switch {
		case !validLabelName(ln):
			return 0, fmt.Errorf("invalid label name %q", ln)
		case strings.HasPrefix(ln, "__"):
			return 0, fmt.Errorf("reserved label name %q", ln)
		}
		if _, dup := labels[ln]; dup {
			return 0, fmt.Errorf("duplicate label %q", ln)
		}
		i += eq + 1
		if i >= len(line) || line[i] != '"' {
			return 0, fmt.Errorf("label %s: value is not quoted", ln)
		}
		i++
		var val strings.Builder
		closed := false
		for !closed {
			if i >= len(line) {
				return 0, fmt.Errorf("label %s: unterminated value", ln)
			}
			switch c := line[i]; c {
			case '"':
				closed = true
			case '\\':
				if i+1 >= len(line) {
					return 0, fmt.Errorf("label %s: dangling backslash", ln)
				}
				switch line[i+1] {
				case '\\':
					val.WriteByte('\\')
				case '"':
					val.WriteByte('"')
				case 'n':
					val.WriteByte('\n')
				default:
					return 0, fmt.Errorf("label %s: invalid escape \\%c", ln, line[i+1])
				}
				i++
			default:
				val.WriteByte(c)
			}
			i++
		}
		labels[ln] = val.String()
		switch {
		case i < len(line) && line[i] == ',':
			i++
		case i < len(line) && line[i] == '}':
		default:
			return 0, fmt.Errorf("label %s: expected , or } after value", ln)
		}
	}
}

// unescape decodes the HELP escapes \\ and \n; any other backslash sequence is
// an error.
func unescape(s string) (string, error) {
	if !strings.Contains(s, `\`) {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch {
		case i < len(s) && s[i] == '\\':
			b.WriteByte('\\')
		case i < len(s) && s[i] == 'n':
			b.WriteByte('\n')
		default:
			return "", fmt.Errorf("invalid escape in %q", s)
		}
	}
	return b.String(), nil
}

func parseValue(s string) (float64, error) {
	switch s {
	case "+Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	case "NaN":
		return math.NaN(), nil
	}
	if s == "" || strings.Trim(s, "0123456789.eE+-") != "" {
		return 0, fmt.Errorf("invalid value %q", s)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", s)
	}
	return v, nil
}

func validTimestamp(s string) bool {
	s = strings.TrimPrefix(s, "-")
	return s != "" && strings.Trim(s, "0123456789") == ""
}

func validMetricName(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if c == '_' || c == ':' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

func validLabelName(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

// seriesKey is unambiguous for any label content because values are quoted.
func seriesKey(name string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(strconv.Quote(labels[k]))
		b.WriteByte(',')
	}
	b.WriteByte('}')
	return b.String()
}

type histGroup struct {
	key        string
	lastLE     float64
	counts     []float64
	sum, count *float64
}

func checkHistogram(f *Family) error {
	var order []string
	groups := map[string]*histGroup{}
	for _, s := range f.Samples {
		rest := make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			if k != "le" || s.Name != f.Name+"_bucket" {
				rest[k] = v
			}
		}
		key := seriesKey(f.Name, rest)
		g := groups[key]
		if g == nil {
			g = &histGroup{key: key}
			groups[key] = g
			order = append(order, key)
		}
		v := s.Value
		switch s.Name {
		case f.Name + "_bucket":
			leStr, ok := s.Labels["le"]
			if !ok {
				return fmt.Errorf("histogram %s: bucket without le", f.Name)
			}
			le, err := parseValue(leStr)
			if err != nil {
				return fmt.Errorf("histogram %s: le: %w", f.Name, err)
			}
			if len(g.counts) > 0 && !(le > g.lastLE) {
				return fmt.Errorf("histogram %s%s: le %q not ascending", f.Name, key, leStr)
			}
			if n := len(g.counts); n > 0 && !(v >= g.counts[n-1]) {
				return fmt.Errorf("histogram %s%s: bucket counts decrease at le %q", f.Name, key, leStr)
			}
			g.lastLE = le
			g.counts = append(g.counts, v)
		case f.Name + "_sum":
			g.sum = &v
		case f.Name + "_count":
			g.count = &v
		}
	}
	for _, key := range order {
		g := groups[key]
		switch {
		case len(g.counts) == 0:
			return fmt.Errorf("histogram %s%s: no buckets", f.Name, key)
		case !math.IsInf(g.lastLE, 1):
			return fmt.Errorf("histogram %s%s: last bucket is not +Inf", f.Name, key)
		case g.sum == nil:
			return fmt.Errorf("histogram %s%s: missing _sum", f.Name, key)
		case g.count == nil:
			return fmt.Errorf("histogram %s%s: missing _count", f.Name, key)
		case *g.count != g.counts[len(g.counts)-1]:
			return fmt.Errorf("histogram %s%s: _count %v differs from the +Inf bucket %v", f.Name, key, *g.count, g.counts[len(g.counts)-1])
		}
	}
	return nil
}
