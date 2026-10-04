package gateway

import (
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"fmt"
)

// A placeholder stands in for a secret inside a sandbox: the guest holds
// bhatti_sec_<24 base32>, never the value. netd swaps it for the real value
// only in request headers, only on a TLS connection it intercepted for a host
// the secret's grant names. It is unguessable (120 random bits) and useless
// outside that sandbox: the broker resolves it only for the sandbox the grant
// was minted for.

// PlaceholderPrefix starts every placeholder; the fixed shape lets netd spot
// one anywhere in a request without knowing which ones are live.
const PlaceholderPrefix = "bhatti_sec_"

const placeholderRandChars = 24 // 15 random bytes in base32

// PlaceholderLen is the length of every placeholder.
const PlaceholderLen = len(PlaceholderPrefix) + placeholderRandChars

var placeholderEnc = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// NewPlaceholder mints a fresh placeholder.
func NewPlaceholder() (string, error) {
	var b [15]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint placeholder: %w", err)
	}
	return PlaceholderPrefix + placeholderEnc.EncodeToString(b[:]), nil
}

func isPlaceholderChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '2' && c <= '7')
}

// IsPlaceholder reports whether s is exactly one placeholder.
func IsPlaceholder(s string) bool {
	return len(s) == PlaceholderLen && indexPlaceholder([]byte(s)) == 0
}

// indexPlaceholder returns the offset of the first placeholder in b, or -1.
func indexPlaceholder(b []byte) int {
	off := 0
	for {
		i := bytes.Index(b[off:], []byte(PlaceholderPrefix))
		if i < 0 {
			return -1
		}
		start := off + i
		end := start + PlaceholderLen
		if end > len(b) {
			return -1
		}
		ok := true
		for _, c := range b[start+len(PlaceholderPrefix) : end] {
			if !isPlaceholderChar(c) {
				ok = false
				break
			}
		}
		if ok {
			return start
		}
		off = start + 1
	}
}

// FindPlaceholders returns the distinct placeholders in b, in order of first
// appearance.
func FindPlaceholders(b []byte) []string {
	var out []string
	seen := map[string]bool{}
	for {
		i := indexPlaceholder(b)
		if i < 0 {
			return out
		}
		p := string(b[i : i+PlaceholderLen])
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
		b = b[i+PlaceholderLen:]
	}
}

// ContainsPlaceholder reports whether b holds any placeholder.
func ContainsPlaceholder(b []byte) bool { return indexPlaceholder(b) >= 0 }
