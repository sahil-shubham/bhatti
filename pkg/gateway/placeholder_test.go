package gateway

import (
	"bufio"
	"slices"
	"strings"
	"testing"
)

func TestNewPlaceholderShapeAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		p, err := NewPlaceholder()
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != PlaceholderLen || !strings.HasPrefix(p, PlaceholderPrefix) || !IsPlaceholder(p) {
			t.Fatalf("malformed placeholder %q", p)
		}
		if seen[p] {
			t.Fatalf("duplicate placeholder %q", p)
		}
		seen[p] = true
	}
}

// TestFindPlaceholders pins what netd treats as a placeholder: the prefix plus
// exactly the random part's alphabet, wherever it sits.
func TestFindPlaceholders(t *testing.T) {
	a, _ := NewPlaceholder()
	b, _ := NewPlaceholder()
	cases := []struct {
		in   string
		want []string
	}{
		{"Bearer " + a, []string{a}},
		{a + b, []string{a, b}},
		{"x" + a + "y," + a + " " + b, []string{a, b}},
		{`{"k":"` + a + `"}`, []string{a}},
		{PlaceholderPrefix + "short", nil},
		{a[:PlaceholderLen-1], nil},
		{strings.ToUpper(a), nil},
		{PlaceholderPrefix + strings.Repeat("1", 24), nil}, // 0, 1, 8, 9 aren't in the alphabet
		{"bhatti_sec" + a, []string{a}},                    // a near-miss prefix right before a real one
	}
	for _, c := range cases {
		if got := FindPlaceholders([]byte(c.in)); !slices.Equal(got, c.want) {
			t.Errorf("FindPlaceholders(%q) = %q, want %q", c.in, got, c.want)
		}
		if got := ContainsPlaceholder([]byte(c.in)); got != (len(c.want) > 0) {
			t.Errorf("ContainsPlaceholder(%q) = %v", c.in, got)
		}
	}
	if IsPlaceholder(a+"x") || IsPlaceholder(" "+a) {
		t.Error("IsPlaceholder accepted extra bytes")
	}
}

// TestReadBrokerMessageBounded: a request longer than the reader's buffer is
// rejected rather than buffered without limit.
func TestReadBrokerMessageBounded(t *testing.T) {
	line := `{"op":"resolve","host":"` + strings.Repeat("a", BrokerMaxRequest) + "\"}\n"
	var req BrokerRequest
	if err := ReadBrokerMessage(bufio.NewReaderSize(strings.NewReader(line), BrokerMaxRequest), &req); err != ErrBrokerMessageTooLarge {
		t.Fatalf("err = %v, want ErrBrokerMessageTooLarge", err)
	}
	ok := `{"op":"cert","sandbox":"s","host":"api.example.com"}` + "\n"
	if err := ReadBrokerMessage(bufio.NewReaderSize(strings.NewReader(ok), BrokerMaxRequest), &req); err != nil || req.Host != "api.example.com" {
		t.Fatalf("err=%v req=%+v", err, req)
	}
}
