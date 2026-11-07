// Package promtext writes the Prometheus text exposition format (0.0.4).
//
// The Writer refuses to emit anything a strict scraper would reject: a rule
// violation records a sticky error and the offending line is dropped, so the
// bytes that do reach the io.Writer are always a valid exposition.
package promtext

import (
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Type string

const (
	Counter   Type = "counter"
	Gauge     Type = "gauge"
	Histogram Type = "histogram"
	Untyped   Type = "untyped"
)

// autoFlush bounds memory when a caller renders a very large exposition
// through one Writer; Flush semantics are unchanged.
const autoFlush = 64 << 10

type Writer struct {
	w   io.Writer
	buf []byte
	err error // first error seen, rule violation or write failure
	// writeFailed stops further writes after a failed Write.
	writeFailed bool
	started     map[string]struct{}
	fam         string // current family; "" when none or when its header was rejected
	typ         Type
}

// New returns a Writer that buffers internally; call Flush to write.
func New(w io.Writer) *Writer {
	return &Writer{w: w, buf: make([]byte, 0, 4096), started: make(map[string]struct{})}
}

func (w *Writer) fail(format string, args ...any) {
	if w.err == nil {
		w.err = fmt.Errorf("promtext: "+format, args...)
	}
}

// Family starts a metric family: it writes the HELP and TYPE lines, and every
// Sample until the next Family call belongs to it.
func (w *Writer) Family(name, help string, typ Type) {
	// A rejected header leaves no current family so that its samples are
	// rejected too instead of landing under the previous family.
	w.fam, w.typ = "", ""
	switch {
	case !validMetricName(name):
		w.fail("invalid metric name %q", name)
		return
	case typ != Counter && typ != Gauge && typ != Histogram && typ != Untyped:
		w.fail("family %s: invalid type %q", name, typ)
		return
	case typ == Counter && !hasSuffix(name, "_total"):
		w.fail("counter family %s must end in _total", name)
		return
	}
	if _, dup := w.started[name]; dup {
		w.fail("family %s started twice", name)
		return
	}
	w.started[name] = struct{}{}
	w.fam, w.typ = name, typ

	w.buf = append(w.buf, "# HELP "...)
	w.buf = append(w.buf, name...)
	if help != "" {
		w.buf = append(w.buf, ' ')
		w.buf = appendEscaped(w.buf, help, false)
	}
	w.buf = append(w.buf, "\n# TYPE "...)
	w.buf = append(w.buf, name...)
	w.buf = append(w.buf, ' ')
	w.buf = append(w.buf, typ...)
	w.buf = append(w.buf, '\n')
	w.maybeFlush()
}

// Sample writes one sample line. name is the family name or, for a histogram
// family, the family name plus _bucket, _sum or _count. labels are alternating
// name, value pairs, printed in the order given.
func (w *Writer) Sample(name string, value float64, labels ...string) {
	if w.fam == "" {
		w.fail("sample %s outside a valid family", name)
		return
	}
	if !w.belongs(name) {
		w.fail("sample %s does not belong to family %s (%s)", name, w.fam, w.typ)
		return
	}
	if len(labels)%2 != 0 {
		w.fail("sample %s: odd number of label arguments", name)
		return
	}
	hasLE := false
	for i := 0; i < len(labels); i += 2 {
		ln := labels[i]
		switch {
		case !validLabelName(ln):
			w.fail("sample %s: invalid label name %q", name, ln)
			return
		case len(ln) >= 2 && ln[0] == '_' && ln[1] == '_':
			w.fail("sample %s: label name %q is reserved", name, ln)
			return
		}
		for j := 0; j < i; j += 2 {
			if labels[j] == ln {
				w.fail("sample %s: duplicate label %q", name, ln)
				return
			}
		}
		hasLE = hasLE || ln == "le"
	}
	if w.typ == Histogram && hasSuffix(name, "_bucket") && !hasLE {
		w.fail("histogram bucket %s has no le label", name)
		return
	}

	w.buf = append(w.buf, name...)
	if len(labels) > 0 {
		w.buf = append(w.buf, '{')
		for i := 0; i < len(labels); i += 2 {
			if i > 0 {
				w.buf = append(w.buf, ',')
			}
			w.buf = append(w.buf, labels[i]...)
			w.buf = append(w.buf, '=', '"')
			w.buf = appendEscaped(w.buf, labels[i+1], true)
			w.buf = append(w.buf, '"')
		}
		w.buf = append(w.buf, '}')
	}
	w.buf = append(w.buf, ' ')
	w.buf = appendValue(w.buf, value)
	w.buf = append(w.buf, '\n')
	w.maybeFlush()
}

// Flush writes what is buffered and returns the first error seen, either a
// write error or a rule violation. The error is sticky.
func (w *Writer) Flush() error {
	w.flush()
	return w.err
}

func (w *Writer) maybeFlush() {
	if len(w.buf) >= autoFlush {
		w.flush()
	}
}

func (w *Writer) flush() {
	if len(w.buf) == 0 {
		return
	}
	if !w.writeFailed {
		n, err := w.w.Write(w.buf)
		if err == nil && n < len(w.buf) {
			err = io.ErrShortWrite
		}
		if err != nil {
			// A rule violation does not stop output (the offending line was
			// already dropped); a failed write does, so that nothing is
			// written after a gap.
			w.writeFailed = true
			if w.err == nil {
				w.err = errors.Join(errors.New("promtext: write"), err)
			}
		}
	}
	w.buf = w.buf[:0]
}

// belongs reports whether name is a legal sample name in the current family.
// A histogram family has no bare-name sample: its series are the suffixed ones.
func (w *Writer) belongs(name string) bool {
	if w.typ != Histogram {
		return name == w.fam
	}
	if len(name) <= len(w.fam) || name[:len(w.fam)] != w.fam {
		return false
	}
	switch name[len(w.fam):] {
	case "_bucket", "_sum", "_count":
		return true
	}
	return false
}

func hasSuffix(s, suffix string) bool { return strings.HasSuffix(s, suffix) }

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

// appendEscaped escapes backslash and newline, plus the double quote inside a
// label value, and replaces invalid UTF-8 with U+FFFD (the format is UTF-8).
func appendEscaped(b []byte, s string, quote bool) []byte {
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\':
			b = append(b, '\\', '\\')
		case c == '\n':
			b = append(b, '\\', 'n')
		case c == '"' && quote:
			b = append(b, '\\', '"')
		case c < utf8.RuneSelf:
			b = append(b, c)
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				b = utf8.AppendRune(b, utf8.RuneError)
			} else {
				b = append(b, s[i:i+size]...)
			}
			i += size
			continue
		}
		i++
	}
	return b
}

func appendValue(b []byte, v float64) []byte {
	switch {
	case math.IsNaN(v):
		return append(b, "NaN"...)
	case math.IsInf(v, 1):
		return append(b, "+Inf"...)
	case math.IsInf(v, -1):
		return append(b, "-Inf"...)
	}
	return strconv.AppendFloat(b, v, 'g', -1, 64)
}
