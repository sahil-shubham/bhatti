package promtext_test

import (
	"bytes"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg/promtext"
	"github.com/sahil-shubham/bhatti/pkg/promtext/promtexttest"
)

func render(t *testing.T, fn func(w *promtext.Writer)) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	w := promtext.New(&buf)
	fn(w)
	err := w.Flush()
	return buf.String(), err
}

func parse(t *testing.T, s string) *promtexttest.Exposition {
	t.Helper()
	e, err := promtexttest.Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("writer output rejected by the strict parser: %v\n%s", err, s)
	}
	return e
}

func TestEscaping(t *testing.T) {
	out, err := render(t, func(w *promtext.Writer) {
		w.Family("m", "back\\slash \"quoted\"\nnew\xffline", promtext.Gauge)
		w.Sample("m", 1, "l", "a\\b \"q\"\nc \xff\xfe d é")
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "# HELP m back\\\\slash \"quoted\"\\nnew\uFFFDline\n" +
		"# TYPE m gauge\n" +
		"m{l=\"a\\\\b \\\"q\\\"\\nc \uFFFD\uFFFD d é\"} 1\n"
	if out != want {
		t.Errorf("got\n%q\nwant\n%q", out, want)
	}
	e := parse(t, out)
	f := e.Family("m")
	if f.Help != "back\\slash \"quoted\"\nnew\uFFFDline" {
		t.Errorf("help round trip = %q", f.Help)
	}
	if v, ok := f.Value("l", "a\\b \"q\"\nc \uFFFD\uFFFD d é"); !ok || v != 1 {
		t.Errorf("label round trip failed: %+v", f.Samples)
	}
}

func TestEmptyHelpAndNoLabels(t *testing.T) {
	out, err := render(t, func(w *promtext.Writer) {
		w.Family("m", "", promtext.Untyped)
		w.Sample("m", 2)
		w.Sample("m", 3, "k", "")
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "# HELP m\n# TYPE m untyped\nm 2\nm{k=\"\"} 3\n"; out != want {
		t.Errorf("got %q want %q", out, want)
	}
	parse(t, out)
}

func TestValuesRoundTrip(t *testing.T) {
	vals := []float64{
		0, math.Copysign(0, -1), 1, -1, 0.1, 1.5, 100, 123456789, 1e15, 1e21, 1e-7,
		math.MaxFloat64, math.SmallestNonzeroFloat64, -math.MaxFloat64,
		math.Inf(1), math.Inf(-1), math.NaN(), float64(1<<53 + 1),
	}
	out, err := render(t, func(w *promtext.Writer) {
		w.Family("v", "values", promtext.Gauge)
		for i, v := range vals {
			w.Sample("v", v, "i", strconv.Itoa(i))
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	f := parse(t, out).Family("v")
	for i, want := range vals {
		got, ok := f.Value("i", strconv.Itoa(i))
		if !ok {
			t.Fatalf("sample %d missing", i)
		}
		if math.IsNaN(want) != math.IsNaN(got) || !math.IsNaN(want) && math.Float64bits(want) != math.Float64bits(got) {
			t.Errorf("value %d: wrote %v, parsed %v", i, want, got)
		}
	}
	for _, lit := range []string{"} NaN\n", "} +Inf\n", "} -Inf\n", "} -0\n"} {
		if !strings.Contains(out, lit) {
			t.Errorf("output lacks %q:\n%s", lit, out)
		}
	}
}

func TestRuleViolations(t *testing.T) {
	cases := []struct {
		name string
		// bad runs one violating call after a valid family "ok" with one sample.
		bad func(w *promtext.Writer)
		// absent is text that must not appear in the output.
		absent string
	}{
		{"metric name with dash", func(w *promtext.Writer) { w.Sample("ok-x", 1) }, "ok-x"},
		{"label name with dash", func(w *promtext.Writer) { w.Sample("ok", 1, "a-b", "v") }, "a-b"},
		{"label name empty", func(w *promtext.Writer) { w.Sample("ok", 1, "", "v") }, "=\"v\""},
		{"label name starts with digit", func(w *promtext.Writer) { w.Sample("ok", 1, "1a", "v") }, "1a"},
		{"label name with colon", func(w *promtext.Writer) { w.Sample("ok", 1, "a:b", "v") }, "a:b"},
		{"reserved label name", func(w *promtext.Writer) { w.Sample("ok", 1, "__name__", "v") }, "__name__"},
		{"repeated label name", func(w *promtext.Writer) { w.Sample("ok", 1, "a", "1", "b", "2", "a", "3") }, "b=\"2\""},
		{"odd label arguments", func(w *promtext.Writer) { w.Sample("ok", 1, "a", "1", "b") }, "a=\"1\""},
		{"sample of another name", func(w *promtext.Writer) { w.Sample("other", 1) }, "other"},
		{"sample with suffix on a gauge", func(w *promtext.Writer) { w.Sample("ok_count", 1) }, "ok_count"},
		{"family with invalid name", func(w *promtext.Writer) { w.Family("a b", "h", promtext.Gauge) }, "a b"},
		{"family with empty name", func(w *promtext.Writer) { w.Family("", "h", promtext.Gauge) }, "# TYPE  "},
		{"family with unknown type", func(w *promtext.Writer) { w.Family("fam", "h", "summary") }, "fam"},
		{"family started twice", func(w *promtext.Writer) { w.Family("ok", "h", promtext.Gauge) }, "# HELP ok h"},
		{"counter without _total", func(w *promtext.Writer) { w.Family("ctr", "h", promtext.Counter) }, "ctr"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := render(t, func(w *promtext.Writer) {
				w.Family("ok", "good", promtext.Gauge)
				w.Sample("ok", 1)
				c.bad(w)
			})
			if err == nil {
				t.Fatalf("no error for a rule violation; output:\n%s", out)
			}
			if strings.Contains(out, c.absent) {
				t.Errorf("offending text %q was emitted:\n%s", c.absent, out)
			}
			if out != "# HELP ok good\n# TYPE ok gauge\nok 1\n" {
				t.Errorf("valid prefix altered or extra output: %q", out)
			}
			parse(t, out)
		})
	}
}

func TestSampleBeforeFamilyIsRejected(t *testing.T) {
	out, err := render(t, func(w *promtext.Writer) { w.Sample("m", 1) })
	if err == nil || out != "" {
		t.Errorf("err=%v out=%q", err, out)
	}
}

func TestRejectedFamilyDoesNotAdoptSamples(t *testing.T) {
	// If a failed header left the previous family current, the samples meant
	// for the failed family would be silently emitted under the wrong one.
	for name, bad := range map[string]func(w *promtext.Writer){
		"invalid name":  func(w *promtext.Writer) { w.Family("bad name", "h", promtext.Gauge) },
		"duplicate":     func(w *promtext.Writer) { w.Family("first", "h", promtext.Gauge) },
		"bad counter":   func(w *promtext.Writer) { w.Family("ctr", "h", promtext.Counter) },
		"unknown type":  func(w *promtext.Writer) { w.Family("t", "h", "bogus") },
		"first-in-test": func(w *promtext.Writer) { w.Family("1x", "h", promtext.Gauge) },
	} {
		t.Run(name, func(t *testing.T) {
			out, err := render(t, func(w *promtext.Writer) {
				w.Family("first", "h", promtext.Gauge)
				w.Sample("first", 1)
				bad(w)
				w.Sample("first", 2, "late", "yes")
			})
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(out, "late") {
				t.Errorf("sample landed in the wrong family:\n%s", out)
			}
		})
	}
}

func TestFamiliesAreContiguousAndUnique(t *testing.T) {
	out, err := render(t, func(w *promtext.Writer) {
		w.Family("a", "h", promtext.Gauge)
		w.Sample("a", 1)
		w.Family("b", "h", promtext.Gauge)
		w.Sample("b", 1)
		w.Sample("a", 2, "again", "1") // a is no longer current
	})
	if err == nil || strings.Contains(out, "again") {
		t.Errorf("err=%v, output:\n%s", err, out)
	}
	parse(t, out)
}

func TestErrorIsSticky(t *testing.T) {
	var buf bytes.Buffer
	w := promtext.New(&buf)
	w.Family("ok", "h", promtext.Gauge)
	w.Sample("ok", 1, "odd")
	first := w.Flush()
	if first == nil {
		t.Fatal("expected an error")
	}
	w.Sample("ok", 2) // a valid sample after the error is still written
	second := w.Flush()
	if second == nil || second.Error() != first.Error() {
		t.Errorf("error not sticky: first=%v second=%v", first, second)
	}
	w.Sample("other", 3) // a later violation must not replace the first error
	if err := w.Flush(); err == nil || err.Error() != first.Error() {
		t.Errorf("first error replaced: %v", err)
	}
	if got, want := buf.String(), "# HELP ok h\n# TYPE ok gauge\nok 2\n"; got != want {
		t.Errorf("output %q want %q", got, want)
	}
}

func TestCounterNaming(t *testing.T) {
	out, err := render(t, func(w *promtext.Writer) {
		w.Family("reqs_total", "h", promtext.Counter)
		w.Sample("reqs_total", 5, "code", "200")
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := parse(t, out).Family("reqs_total").Value("code", "200"); !ok || v != 5 {
		t.Errorf("counter sample = %v %v", v, ok)
	}
}

func TestHistogramFamily(t *testing.T) {
	out, err := render(t, func(w *promtext.Writer) {
		w.Family("lat_seconds", "Latency.", promtext.Histogram)
		for _, op := range []string{"read", "write"} {
			w.Sample("lat_seconds_bucket", 1, "op", op, "le", "0.1")
			w.Sample("lat_seconds_bucket", 4, "op", op, "le", "1")
			w.Sample("lat_seconds_bucket", 4, "op", op, "le", "+Inf")
			w.Sample("lat_seconds_sum", 2.5, "op", op)
			w.Sample("lat_seconds_count", 4, "op", op)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	e := parse(t, out)
	f := e.Family("lat_seconds")
	if f == nil || f.Type != "histogram" || len(f.Samples) != 10 {
		t.Fatalf("family = %+v", f)
	}
	if s, ok := f.Find(map[string]string{"op": "write", "le": "+Inf"}); !ok || s.Name != "lat_seconds_bucket" || s.Value != 4 {
		t.Errorf("+Inf bucket = %+v %v", s, ok)
	}
	if !strings.Contains(out, "lat_seconds_bucket{op=\"read\",le=\"0.1\"} 1\n") {
		t.Errorf("labels not printed in the given order:\n%s", out)
	}
}

func TestHistogramRules(t *testing.T) {
	for name, bad := range map[string]func(w *promtext.Writer){
		"bare family name": func(w *promtext.Writer) { w.Sample("h", 1) },
		"bucket no le":     func(w *promtext.Writer) { w.Sample("h_bucket", 1, "op", "x") },
		"unknown suffix":   func(w *promtext.Writer) { w.Sample("h_total", 1) },
		"longer name":      func(w *promtext.Writer) { w.Sample("h_bucket_x", 1, "le", "1") },
	} {
		t.Run(name, func(t *testing.T) {
			out, err := render(t, func(w *promtext.Writer) {
				w.Family("h", "h", promtext.Histogram)
				bad(w)
			})
			if err == nil || strings.Contains(out, "\nh") {
				t.Errorf("err=%v out=%q", err, out)
			}
		})
	}
}

type failWriter struct {
	err   error
	short bool
	calls int
}

func (f *failWriter) Write(p []byte) (int, error) {
	f.calls++
	if f.short {
		return len(p) - 1, nil
	}
	return 0, f.err
}

func TestWriteErrors(t *testing.T) {
	boom := errors.New("disk full")
	for name, fw := range map[string]*failWriter{
		"error":       {err: boom},
		"short write": {short: true},
	} {
		t.Run(name, func(t *testing.T) {
			w := promtext.New(fw)
			w.Family("a", "h", promtext.Gauge)
			w.Sample("a", 1)
			err := w.Flush()
			if err == nil || (name == "error" && !errors.Is(err, boom)) || (name != "error" && !errors.Is(err, io.ErrShortWrite)) {
				t.Fatalf("err = %v", err)
			}
			calls := fw.calls
			w.Sample("a", 2, "x", "y")
			if err2 := w.Flush(); err2 == nil || err2.Error() != err.Error() {
				t.Errorf("write error not sticky: %v", err2)
			}
			if fw.calls != calls {
				t.Error("kept writing after a failed write")
			}
		})
	}
}

func TestLargeOutputIsFlushedIncrementally(t *testing.T) {
	var buf bytes.Buffer
	w := promtext.New(&buf)
	const n = 20000
	w.Family("big", "h", promtext.Gauge)
	for i := range n {
		w.Sample("big", float64(i), "id", strconv.Itoa(i), "pad", strings.Repeat("x", 20))
	}
	if buf.Len() == 0 {
		t.Error("nothing reached the writer before Flush; the buffer is unbounded")
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := len(parse(t, buf.String()).Family("big").Samples); got != n {
		t.Errorf("parsed %d samples, want %d", got, n)
	}
}

func TestSampleDoesNotAllocate(t *testing.T) {
	w := promtext.New(io.Discard)
	w.Family("m", "h", promtext.Gauge)
	for range 1000 { // grow the buffer to steady state
		w.Sample("m", 1.25, "sandbox", "abc", "owner", "def")
	}
	if got := testing.AllocsPerRun(100, func() {
		w.Sample("m", 1.25, "sandbox", "abc\n\"", "owner", "def")
	}); got != 0 {
		t.Errorf("Sample allocates %v times per call", got)
	}
}

// expected renders what the writer promises for s: invalid UTF-8 becomes U+FFFD
// per offending byte, everything else is preserved.
func expected(s string) string {
	var b strings.Builder
	for _, r := range s {
		b.WriteRune(r)
	}
	return b.String()
}

func checkLabelRoundTrip(t *testing.T, vals []string) {
	t.Helper()
	out, err := render(t, func(w *promtext.Writer) {
		w.Family("rt", "rt \x00 help", promtext.Gauge)
		for i, v := range vals {
			w.Sample("rt", float64(i), "id", strconv.Itoa(i), "v", v)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	f := parse(t, out).Family("rt")
	if len(f.Samples) != len(vals) {
		t.Fatalf("parsed %d samples, wrote %d", len(f.Samples), len(vals))
	}
	for i, v := range vals {
		got, ok := f.Find(map[string]string{"id": strconv.Itoa(i)})
		if !ok || got.Labels["v"] != expected(v) || got.Value != float64(i) {
			t.Fatalf("value %q: got %+v", v, got)
		}
	}
}

func TestRandomLabelValuesRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	vals := []string{"", "\\", "\"", "\n", "\\n", "\\\"", "\xff", "\xc3", "\xe2\x82", "{}=,# ", "\r", "\x00", "日本\u2028"}
	for range 500 {
		b := make([]byte, rng.IntN(40))
		for i := range b {
			b[i] = byte(rng.IntN(256))
		}
		vals = append(vals, string(b))
	}
	checkLabelRoundTrip(t, vals)
}

func FuzzLabelValueRoundTrip(f *testing.F) {
	f.Add("plain")
	f.Add("a\\b\"c\nd")
	f.Add("\xff\xfe")
	f.Fuzz(func(t *testing.T, s string) {
		checkLabelRoundTrip(t, []string{s})
	})
}

func TestMixedFamiliesRoundTrip(t *testing.T) {
	type want struct {
		name   string
		labels map[string]string
		value  float64
	}
	var wants []want
	out, err := render(t, func(w *promtext.Writer) {
		put := func(name string, v float64, kv ...string) {
			m := map[string]string{}
			for i := 0; i < len(kv); i += 2 {
				m[kv[i]] = kv[i+1]
			}
			wants = append(wants, want{name, m, v})
			w.Sample(name, v, kv...)
		}
		w.Family("cpu_seconds_total", "CPU.", promtext.Counter)
		put("cpu_seconds_total", 12.5, "sandbox", "s1", "owner", "o1")
		put("cpu_seconds_total", 0, "sandbox", "s2", "owner", "o1")
		w.Family("mem_bytes", "Memory.", promtext.Gauge)
		put("mem_bytes", 1<<30, "sandbox", "s1")
		put("mem_bytes", math.NaN(), "sandbox", "s2")
		w.Family("untyped_thing", "", promtext.Untyped)
		put("untyped_thing", math.Inf(-1))
		w.Family("d_seconds", "Duration.", promtext.Histogram)
		put("d_seconds_bucket", 2, "le", "0.5")
		put("d_seconds_bucket", 3, "le", "+Inf")
		put("d_seconds_sum", 1.25)
		put("d_seconds_count", 3)
	})
	if err != nil {
		t.Fatal(err)
	}
	e := parse(t, out)
	var got []promtexttest.Sample
	for _, f := range e.Families {
		got = append(got, f.Samples...)
	}
	if len(got) != len(wants) {
		t.Fatalf("parsed %d samples, wrote %d", len(got), len(wants))
	}
	for i, w := range wants {
		g := got[i]
		same := g.Name == w.name && len(g.Labels) == len(w.labels) &&
			(g.Value == w.value || math.IsNaN(g.Value) && math.IsNaN(w.value))
		for k, v := range w.labels {
			same = same && g.Labels[k] == v
		}
		if !same {
			t.Errorf("sample %d: wrote %+v, parsed %+v", i, w, g)
		}
	}
}
