package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// TestCompletionAllShells exercises the completion subcommand for every
// supported shell. Pure in-process — no daemon needed. Catches the class
// of regression where ValidArgs and the RunE switch drift apart.
func TestCompletionAllShells(t *testing.T) {
	cases := []struct {
		shell       string
		mustContain string // a token that uniquely identifies the generated script
	}{
		{"bash", "# bash completion"},
		{"zsh", "#compdef"},
		{"fish", "# fish completion"},
		{"powershell", "powershell completion"},
	}

	for _, tc := range cases {
		t.Run(tc.shell, func(t *testing.T) {
			out := captureStdout(t, func() {
				rootCmd.SetArgs([]string{"completion", tc.shell})
				if err := rootCmd.Execute(); err != nil {
					t.Fatalf("completion %s: %v", tc.shell, err)
				}
			})
			if !strings.Contains(out, tc.mustContain) {
				snippet := out
				if len(snippet) > 200 {
					snippet = snippet[:200]
				}
				t.Errorf("completion %s missing %q\nfirst 200 chars: %q",
					tc.shell, tc.mustContain, snippet)
			}
		})
	}
}

// TestCompletionValidArgs locks the shell set as part of the public
// surface. Adding or removing a shell must be deliberate, not drift.
// Order-insensitive: cobra may reorder ValidArgs internally.
func TestCompletionValidArgs(t *testing.T) {
	want := map[string]bool{"bash": true, "zsh": true, "fish": true, "powershell": true}
	got := make(map[string]bool, len(completionCmd.ValidArgs))
	for _, s := range completionCmd.ValidArgs {
		got[s] = true
	}
	for s := range want {
		if !got[s] {
			t.Errorf("ValidArgs missing %q (have %v)", s, completionCmd.ValidArgs)
		}
	}
	for s := range got {
		if !want[s] {
			t.Errorf("ValidArgs has unexpected %q (have %v)", s, completionCmd.ValidArgs)
		}
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// what was written. cobra's completion generators write to os.Stdout
// directly, so SetOut on the command isn't enough — we have to swap fds.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()

	fn()

	w.Close()
	os.Stdout = orig
	return <-done
}
