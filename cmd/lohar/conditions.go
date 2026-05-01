//go:build linux

package main

import (
	"fmt"
	"os"
	"strings"
)

// evaluateConditions evaluates the [Unit] Condition*= directives on a
// unit. Returns (true, "") if all conditions pass and the unit should
// start; (false, reason) if any condition fails and the unit should be
// skipped without an error.
//
// "Skipped without an error" is the systemd semantic: a failed
// condition is not a failure -- the unit just doesn't activate this
// time. Logged but not surfaced as is-failed. systemctl status shows
// "condition failed" or similar.
//
// The directives systemd supports are extensive. We implement the four
// real-world packages actually use:
//
//	ConditionPathExists=PATH        (! prefix negates)
//	ConditionPathIsDirectory=PATH   (! prefix negates)
//	ConditionDirectoryNotEmpty=PATH (! prefix negates)
//	ConditionFileNotEmpty=PATH      (! prefix negates)
//
// Concrete user-visible scenario this enables: real ssh.service has
//
//	ConditionPathExists=!/etc/ssh/sshd_not_to_be_run
//
// An admin who touches that file expects sshd to skip-not-run on next
// activation. Without F4 the directive was silently ignored and sshd
// started anyway -- a subtle "this is just lohar lying" footgun.
//
// Assert*= directives (which FAIL the unit on violation, vs Condition
// which silently skips) are deferred. They're rare in package-shipped
// units; mostly bespoke admin units use them. Add when a real case
// arises.
func evaluateConditions(u *Unit) (bool, string) {
	if u == nil {
		return true, ""
	}
	for _, c := range u.Sections.getAll("Unit", "ConditionPathExists") {
		if ok, reason := evalPathExists(c); !ok {
			return false, reason
		}
	}
	for _, c := range u.Sections.getAll("Unit", "ConditionPathIsDirectory") {
		if ok, reason := evalPathIsDirectory(c); !ok {
			return false, reason
		}
	}
	for _, c := range u.Sections.getAll("Unit", "ConditionDirectoryNotEmpty") {
		if ok, reason := evalDirectoryNotEmpty(c); !ok {
			return false, reason
		}
	}
	for _, c := range u.Sections.getAll("Unit", "ConditionFileNotEmpty") {
		if ok, reason := evalFileNotEmpty(c); !ok {
			return false, reason
		}
	}
	return true, ""
}

// stripNegation handles the "!PATH" form. Returns (path, negate).
// Empty path is reported as no-op (always passes); systemd treats an
// empty Condition*= as a reset of any previously-accumulated conditions
// of the same type, but for our use case "always pass" is good enough
// because we don't accumulate cross-fragment conditions.
func stripNegation(raw string) (path string, negate bool) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "!") {
		return strings.TrimSpace(raw[1:]), true
	}
	return raw, false
}

func evalPathExists(raw string) (bool, string) {
	path, negate := stripNegation(raw)
	if path == "" {
		return true, ""
	}
	_, err := os.Stat(path)
	exists := err == nil
	if negate {
		if exists {
			return false, fmt.Sprintf("ConditionPathExists=!%s failed (path exists)", path)
		}
		return true, ""
	}
	if !exists {
		return false, fmt.Sprintf("ConditionPathExists=%s failed (path absent)", path)
	}
	return true, ""
}

func evalPathIsDirectory(raw string) (bool, string) {
	path, negate := stripNegation(raw)
	if path == "" {
		return true, ""
	}
	st, err := os.Stat(path)
	isDir := err == nil && st.IsDir()
	if negate {
		if isDir {
			return false, fmt.Sprintf("ConditionPathIsDirectory=!%s failed (is a directory)", path)
		}
		return true, ""
	}
	if !isDir {
		return false, fmt.Sprintf("ConditionPathIsDirectory=%s failed (not a directory)", path)
	}
	return true, ""
}

func evalDirectoryNotEmpty(raw string) (bool, string) {
	path, negate := stripNegation(raw)
	if path == "" {
		return true, ""
	}
	entries, err := os.ReadDir(path)
	notEmpty := err == nil && len(entries) > 0
	if negate {
		if notEmpty {
			return false, fmt.Sprintf("ConditionDirectoryNotEmpty=!%s failed (directory not empty)", path)
		}
		return true, ""
	}
	if !notEmpty {
		return false, fmt.Sprintf("ConditionDirectoryNotEmpty=%s failed (directory empty or missing)", path)
	}
	return true, ""
}

func evalFileNotEmpty(raw string) (bool, string) {
	path, negate := stripNegation(raw)
	if path == "" {
		return true, ""
	}
	st, err := os.Stat(path)
	notEmpty := err == nil && !st.IsDir() && st.Size() > 0
	if negate {
		if notEmpty {
			return false, fmt.Sprintf("ConditionFileNotEmpty=!%s failed (file not empty)", path)
		}
		return true, ""
	}
	if !notEmpty {
		return false, fmt.Sprintf("ConditionFileNotEmpty=%s failed (file empty or missing)", path)
	}
	return true, ""
}
