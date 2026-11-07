package promtexttest

import (
	"math"
	"strings"
	"testing"
)

func mustParse(t *testing.T, in string) *Exposition {
	t.Helper()
	e, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse rejected valid input: %v\n%s", err, in)
	}
	return e
}

func TestParseAcceptsValidExposition(t *testing.T) {
	e := mustParse(t, ""+
		"# HELP req_total Requests \\\\ served\\nsecond line\n"+
		"# TYPE req_total counter\n"+
		"req_total{path=\"/a\\\\b\",q=\"say \\\"hi\\\"\\nbye\"} 12 1700000000\n"+
		"req_total{path=\"/c\",} 3\n"+
		"# HELP temp \n"+
		"# TYPE temp gauge\n"+
		"temp -1.5e-3\n"+
		"# HELP nohelp_text\n"+
		"# TYPE nohelp_text untyped\n"+
		"nohelp_text +Inf\n"+
		"# HELP lat Latency\n"+
		"# TYPE lat histogram\n"+
		"lat_bucket{op=\"x\",le=\"0.1\"} 1\n"+
		"lat_bucket{op=\"x\",le=\"+Inf\"} 3\n"+
		"lat_sum{op=\"x\"} 0.7\n"+
		"lat_count{op=\"x\"} 3\n"+
		"lat_bucket{op=\"y\",le=\"+Inf\"} 0\n"+
		"lat_sum{op=\"y\"} 0\n"+
		"lat_count{op=\"y\"} 0\n"+
		"# HELP rpc Summary\n"+
		"# TYPE rpc summary\n"+
		"rpc{quantile=\"0.5\"} NaN\n"+
		"rpc_sum 1\n"+
		"rpc_count 2\n")

	req := e.Family("req_total")
	if req == nil || req.Type != "counter" || req.Help != "Requests \\ served\nsecond line" {
		t.Fatalf("req_total family = %+v", req)
	}
	if v, ok := req.Value("path", "/a\\b", "q", "say \"hi\"\nbye"); !ok || v != 12 {
		t.Errorf("escaped labels did not decode: %v %v", v, ok)
	}
	if v, ok := req.Value("path", "/c"); !ok || v != 3 {
		t.Errorf("trailing-comma sample = %v %v", v, ok)
	}
	if _, ok := req.Value("path", "/c", "extra", "x"); ok {
		t.Error("Value matched a label the sample does not have")
	}
	if s, ok := req.Find(map[string]string{}); !ok || s.Value != 12 {
		t.Errorf("Find(empty) should return the first sample, got %v %v", s, ok)
	}
	if v, ok := e.Family("temp").Value(); !ok || v != -0.0015 {
		t.Errorf("temp = %v %v", v, ok)
	}
	if v, _ := e.Family("nohelp_text").Value(); !math.IsInf(v, 1) {
		t.Errorf("+Inf parsed as %v", v)
	}
	if v, _ := e.Family("rpc").Value("quantile", "0.5"); !math.IsNaN(v) {
		t.Errorf("NaN parsed as %v", v)
	}
	if e.Family("absent") != nil {
		t.Error("Family returned a family for an unknown name")
	}
	if got := len(e.Family("lat").Samples); got != 7 {
		t.Errorf("lat has %d samples, want 7", got)
	}
}

func TestParseEmptyInput(t *testing.T) {
	if e := mustParse(t, ""); len(e.Families) != 0 {
		t.Errorf("empty input produced %d families", len(e.Families))
	}
}

func TestParseKeepsFamilyOrderAndHeaderOnlyFamilies(t *testing.T) {
	e := mustParse(t, "# HELP b h\n# TYPE b gauge\n# HELP a h\n# TYPE a gauge\na 1\n")
	if len(e.Families) != 2 || e.Families[0].Name != "b" || len(e.Families[0].Samples) != 0 || e.Families[1].Name != "a" {
		t.Errorf("families = %+v", e.Families)
	}
}

func TestParseRejects(t *testing.T) {
	const g = "# HELP a h\n# TYPE a gauge\n"
	hist := func(body string) string {
		return "# HELP h h\n# TYPE h histogram\n" + body
	}
	cases := []struct{ name, in, want string }{
		{"no trailing newline", g + "a 1", "newline"},
		{"header without trailing newline", "# HELP a h", "newline"},
		{"blank line in the middle", g + "a 1\n\nb 2\n", "blank"},
		{"blank line at the start", "\n" + g + "a 1\n", "blank"},
		{"only a newline", "\n", "blank"},
		{"carriage return", g + "a 1\r\n", "carriage"},
		{"invalid UTF-8", g + "a{x=\"\xff\"} 1\n", "UTF-8"},

		{"duplicate HELP", "# HELP a h\n# HELP a h\n# TYPE a gauge\n", "duplicate HELP"},
		{"duplicate TYPE", g + "# TYPE a gauge\n", "TYPE"},
		{"TYPE before HELP", "# TYPE a gauge\n# HELP a h\n", "without a preceding HELP"},
		{"TYPE after first sample", g + "a 1\n# TYPE a gauge\n", "first sample"},
		{"HELP after first sample", g + "a 1\n# HELP a h\n", "first sample"},
		{"HELP after another family began", g + "# HELP b h\n# TYPE b gauge\n# HELP a h\n", "another family"},
		{"sample after another family", g + "# HELP b h\n# TYPE b gauge\na 1\nb 2\n", "another family"},
		{"family split by another", g + "a 1\n# HELP b h\n# TYPE b gauge\nb 1\na{x=\"1\"} 2\n", "another family"},
		{"sample without any header", "a 1\n", "no HELP/TYPE"},
		{"sample with HELP only", "# HELP a h\na 1\n", "before the TYPE"},
		{"sample of an undeclared name", g + "b 1\n", "no HELP/TYPE"},
		{"TYPE without a type", "# HELP a h\n# TYPE a\n", "without a type"},
		{"unknown type", "# HELP a h\n# TYPE a stateset\n", "unknown type"},
		{"type with trailing text", "# HELP a h\n# TYPE a gauge extra\n", "unknown type"},
		{"counter without _total", "# HELP a h\n# TYPE a counter\n", "_total"},
		{"comment line", "# hello\n", "neither HELP nor TYPE"},
		{"UNIT comment", g + "# UNIT a seconds\n", "neither HELP nor TYPE"},
		{"comment without space", "#HELP a h\n", "neither HELP nor TYPE"},
		{"bad HELP escape", "# HELP a bad \\t escape\n# TYPE a gauge\n", "escape"},
		{"dangling HELP backslash", "# HELP a bad\\\n# TYPE a gauge\n", "escape"},
		{"invalid family name in HELP", "# HELP 1a h\n", "metric name"},

		{"invalid sample name", g + "a-b 1\n", "metric name"},
		{"name starting with digit", "1a 1\n", "metric name"},
		{"invalid label name", g + "a{1x=\"v\"} 1\n", "label name"},
		{"label name with colon", g + "a{x:y=\"v\"} 1\n", "label name"},
		{"reserved label name", g + "a{__x=\"v\"} 1\n", "reserved"},
		{"unquoted label value", g + "a{x=v} 1\n", "not quoted"},
		{"label without equals", g + "a{x} 1\n", "="},
		{"invalid label escape", g + "a{x=\"a\\tb\"} 1\n", "escape"},
		{"dangling label backslash", g + "a{x=\"a\\\"} 1\n", "unterminated"},
		{"unterminated label value", g + "a{x=\"a} 1\n", "unterminated"},
		{"unterminated label set", g + "a{x=\"a\"\n", "expected ,"},
		{"raw newline in label value", g + "a{x=\"a\nb\"} 1\n", ""},
		{"missing comma between labels", g + "a{x=\"1\"y=\"2\"} 1\n", "expected ,"},
		{"single-quoted label value", g + "a{x='v'} 1\n", "not quoted"},
		{"duplicate label name", g + "a{x=\"1\",x=\"2\"} 1\n", "duplicate label"},
		{"duplicate series", g + "a{x=\"1\"} 1\na{x=\"1\"} 2\n", "duplicate series"},
		{"duplicate series reordered labels", g + "a{x=\"1\",y=\"2\"} 1\na{y=\"2\",x=\"1\"} 2\n", "duplicate series"},
		{"duplicate series without labels", g + "a 1\na 2\n", "duplicate series"},

		{"no value", g + "a\n", "no value"},
		{"empty value", g + "a \n", "invalid value"},
		{"label set without value", g + "a{x=\"1\"}\n", "single space"},
		{"two spaces before value", g + "a  1\n", "invalid value"},
		{"non-numeric value", g + "a abc\n", "invalid value"},
		{"hex float", g + "a 0x10\n", "invalid value"},
		{"lowercase inf", g + "a inf\n", "invalid value"},
		{"unsigned Inf", g + "a Inf\n", "invalid value"},
		{"lowercase nan", g + "a nan\n", "invalid value"},
		{"underscore digits", g + "a 1_0\n", "invalid value"},
		{"value out of range", g + "a 1e999\n", "invalid value"},
		{"trailing garbage", g + "a 1 2x\n", "timestamp"},
		{"fractional timestamp", g + "a 1 2.5\n", "timestamp"},
		{"trailing space", g + "a 1 \n", "timestamp"},
		{"two timestamps", g + "a 1 2 3\n", "timestamp"},
		{"tab before value", g + "a\t1\n", "no value"},

		{"le not ascending", hist("h_bucket{le=\"2\"} 1\nh_bucket{le=\"1\"} 2\nh_bucket{le=\"+Inf\"} 2\nh_sum 1\nh_count 2\n"), "ascending"},
		{"le repeated", hist("h_bucket{le=\"1\"} 1\nh_bucket{le=\"1.0\"} 1\nh_bucket{le=\"+Inf\"} 1\nh_sum 1\nh_count 1\n"), "ascending"},
		{"bucket after +Inf", hist("h_bucket{le=\"+Inf\"} 1\nh_bucket{le=\"5\"} 1\nh_sum 1\nh_count 1\n"), "ascending"},
		{"missing +Inf bucket", hist("h_bucket{le=\"1\"} 1\nh_sum 1\nh_count 1\n"), "+Inf"},
		{"decreasing counts", hist("h_bucket{le=\"1\"} 5\nh_bucket{le=\"+Inf\"} 4\nh_sum 1\nh_count 4\n"), "decrease"},
		{"count differs from +Inf bucket", hist("h_bucket{le=\"+Inf\"} 4\nh_sum 1\nh_count 5\n"), "_count"},
		{"missing _count", hist("h_bucket{le=\"+Inf\"} 4\nh_sum 1\n"), "_count"},
		{"missing _sum", hist("h_bucket{le=\"+Inf\"} 4\nh_count 4\n"), "_sum"},
		{"bucket without le", hist("h_bucket 4\nh_sum 1\nh_count 4\n"), "le"},
		{"le not a number", hist("h_bucket{le=\"big\"} 4\nh_sum 1\nh_count 4\n"), "le"},
		{"only _sum and _count", hist("h_sum 1\nh_count 4\n"), "no buckets"},
		{"bare histogram name", hist("h 1\n"), "no HELP/TYPE"},
		{"one label set broken among good ones",
			hist("h_bucket{o=\"a\",le=\"+Inf\"} 1\nh_sum{o=\"a\"} 1\nh_count{o=\"a\"} 1\nh_bucket{o=\"b\",le=\"+Inf\"} 1\nh_sum{o=\"b\"} 1\nh_count{o=\"b\"} 2\n"), "_count"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(c.in))
			if err == nil {
				t.Fatalf("Parse accepted:\n%s", c.in)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("rejected for the wrong reason: %v (want it to mention %q)", err, c.want)
			}
		})
	}
}

func TestParseHistogramLabelSetsAreIndependent(t *testing.T) {
	// Counts of one label set must not be compared with another's.
	mustParse(t, "# HELP h h\n# TYPE h histogram\n"+
		"h_bucket{o=\"a\",le=\"1\"} 9\nh_bucket{o=\"a\",le=\"+Inf\"} 9\nh_sum{o=\"a\"} 1\nh_count{o=\"a\"} 9\n"+
		"h_bucket{o=\"b\",le=\"1\"} 1\nh_bucket{o=\"b\",le=\"+Inf\"} 1\nh_sum{o=\"b\"} 1\nh_count{o=\"b\"} 1\n")
}

func TestValueOddArgumentsPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Value with an odd argument count did not panic")
		}
	}()
	(&Family{}).Value("a")
}
