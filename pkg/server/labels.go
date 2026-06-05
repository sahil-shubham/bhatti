package server

import (
	"fmt"
	"regexp"
	"strings"
)

// Label validation rules, scoped intentionally narrow for v1alpha1:
//
//   - Key:   ^([a-zA-Z0-9]([a-zA-Z0-9._-]{0,61}[a-zA-Z0-9])?/)?[a-zA-Z0-9]([a-zA-Z0-9._-]{0,61}[a-zA-Z0-9])?$
//           optional DNS-subdomain-ish prefix + "/", then the key proper.
//           Each segment ≤ 63 chars. Mirrors what kubectl accepts for
//           label keys minus a few edge cases we don't need.
//   - Value: ≤ 256 chars, runes from [a-zA-Z0-9._-]. Empty is allowed
//           (boolean-style labels: `--label experimental=`).
//   - Max labels per sandbox: 16 (same as kubelet's default node label
//           limit). Larger sets are almost always a misuse.
//   - Reserved key prefix `bhatti.sh/` for future system labels (e.g.
//           bhatti.sh/snapshot-id). User-supplied labels with that
//           prefix are rejected so operators don't accidentally shadow
//           system metadata.

const (
	maxLabelsPerSandbox = 16
	maxLabelValueLen    = 256
	reservedLabelPrefix = "bhatti.sh/"
)

var labelKeyRe = regexp.MustCompile(
	`^([a-zA-Z0-9]([a-zA-Z0-9._-]{0,61}[a-zA-Z0-9])?/)?[a-zA-Z0-9]([a-zA-Z0-9._-]{0,61}[a-zA-Z0-9])?$`,
)

// validateLabel checks a single key/value pair against the v1alpha1
// label rules. Returns a human-readable error or nil.
func validateLabel(key, value string) error {
	if !labelKeyRe.MatchString(key) {
		return fmt.Errorf("invalid label key %q: must match [a-zA-Z0-9._-/]+ (≤63 chars per segment)", key)
	}
	if strings.HasPrefix(key, reservedLabelPrefix) {
		return fmt.Errorf("label key %q uses reserved prefix %q", key, reservedLabelPrefix)
	}
	if len(value) > maxLabelValueLen {
		return fmt.Errorf("label %q value too long: %d > %d", key, len(value), maxLabelValueLen)
	}
	// Empty value allowed (boolean-style). Non-empty must match the
	// same character class as keys (minus the slash, which is reserved
	// for the key's namespace prefix).
	if value != "" && !labelValueRe.MatchString(value) {
		return fmt.Errorf("invalid label value %q for key %q: must match [a-zA-Z0-9._-]+", value, key)
	}
	return nil
}

var labelValueRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// validateLabels checks a whole label map for the create/patch endpoints.
// Returns the first error encountered.
func validateLabels(labels map[string]string) error {
	if len(labels) > maxLabelsPerSandbox {
		return fmt.Errorf("too many labels: %d > %d", len(labels), maxLabelsPerSandbox)
	}
	for k, v := range labels {
		if err := validateLabel(k, v); err != nil {
			return err
		}
	}
	return nil
}

// validateLabelKeys checks a slice of keys (used for the labels_remove
// list in PATCH). Same rules as validateLabel's key portion.
func validateLabelKeys(keys []string) error {
	if len(keys) > maxLabelsPerSandbox {
		return fmt.Errorf("too many label keys to remove: %d > %d", len(keys), maxLabelsPerSandbox)
	}
	for _, k := range keys {
		if !labelKeyRe.MatchString(k) {
			return fmt.Errorf("invalid label key %q", k)
		}
		if strings.HasPrefix(k, reservedLabelPrefix) {
			return fmt.Errorf("label key %q uses reserved prefix %q", k, reservedLabelPrefix)
		}
	}
	return nil
}

// parseLabelQueryParams turns the values of `?label=k=v&label=k2=v2`
// into a map. Each value must be `key=value` (split on the first `=`
// so values may contain further `=` chars). Empty input returns nil
// (matches "no filter" semantics in ListSandboxesWithFilter).
func parseLabelQueryParams(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(values))
	for _, raw := range values {
		eq := strings.IndexByte(raw, '=')
		if eq < 0 {
			return nil, fmt.Errorf("invalid label filter %q: must be key=value", raw)
		}
		key := raw[:eq]
		val := raw[eq+1:]
		if key == "" {
			return nil, fmt.Errorf("invalid label filter %q: empty key", raw)
		}
		// Reject the reserved prefix on filter keys too — operators
		// can't query by system labels via the public API.
		if strings.HasPrefix(key, reservedLabelPrefix) {
			return nil, fmt.Errorf("label key %q uses reserved prefix %q", key, reservedLabelPrefix)
		}
		out[key] = val
	}
	return out, nil
}
