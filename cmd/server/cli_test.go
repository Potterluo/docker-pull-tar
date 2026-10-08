package main

import (
	"testing"
)

// cli_test.go covers the pure argument helpers. The rest of cmd/server is
// I/O (it boots a store and talks to registries), so these helpers are where
// a unit test buys the most: both were written to fix silent-wrong-behaviour
// bugs rather than to be clever.

// TestSplitFlags is the regression test for a bug found by hand:
//
//	dockerpull tasks show <id> --json
//
// Go's flag package stops parsing at the first non-flag argument, so the
// flags after the id were silently dropped — `--json` produced a table,
// `--data-dir` read the wrong database entirely. Silent flag loss is worse
// than a usage error, so args are reordered before parsing.
func TestSplitFlags(t *testing.T) {
	valueFlags := map[string]bool{"data-dir": true}

	cases := []struct {
		name           string
		in             []string
		wantFlags      []string
		wantPositional []string
	}{
		{
			name:           "value flag after a positional",
			in:             []string{"abc123", "--data-dir", "/tmp/x"},
			wantFlags:      []string{"--data-dir", "/tmp/x"},
			wantPositional: []string{"abc123"},
		},
		{
			name:           "bool flags after a positional",
			in:             []string{"abc123", "--json", "--files"},
			wantFlags:      []string{"--json", "--files"},
			wantPositional: []string{"abc123"},
		},
		{
			name:           "mixed order",
			in:             []string{"--json", "abc123", "--data-dir", "/tmp/x", "--files"},
			wantFlags:      []string{"--json", "--data-dir", "/tmp/x", "--files"},
			wantPositional: []string{"abc123"},
		},
		{
			name:           "equals form does not swallow the next token",
			in:             []string{"--data-dir=/tmp/x", "abc123"},
			wantFlags:      []string{"--data-dir=/tmp/x"},
			wantPositional: []string{"abc123"},
		},
		{
			name:           "short flags",
			in:             []string{"abc123", "-json"},
			wantFlags:      []string{"-json"},
			wantPositional: []string{"abc123"},
		},
		{
			name:           "positional only",
			in:             []string{"abc123"},
			wantFlags:      nil,
			wantPositional: []string{"abc123"},
		},
		{
			name:           "flags only",
			in:             []string{"--json"},
			wantFlags:      []string{"--json"},
			wantPositional: nil,
		},
		{
			// A trailing value flag with nothing after it must not panic or
			// invent an argument; flag.Parse reports the real error.
			name:           "trailing value flag",
			in:             []string{"abc123", "--data-dir"},
			wantFlags:      []string{"--data-dir"},
			wantPositional: []string{"abc123"},
		},
		{
			// A lone "-" is a conventional stdin placeholder, not a flag.
			name:           "lone dash is positional",
			in:             []string{"-"},
			wantFlags:      nil,
			wantPositional: []string{"-"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flags, positional := splitFlags(tc.in, valueFlags)
			if !equalStrings(flags, tc.wantFlags) {
				t.Errorf("flags = %v, want %v", flags, tc.wantFlags)
			}
			if !equalStrings(positional, tc.wantPositional) {
				t.Errorf("positional = %v, want %v", positional, tc.wantPositional)
			}
			// Nothing may be lost or duplicated.
			if len(flags)+len(positional) != len(tc.in) {
				t.Errorf("split lost or duplicated tokens: in=%d flags+positional=%d",
					len(tc.in), len(flags)+len(positional))
			}
		})
	}
}

// TestHasReferenceSuffix protects the `--tag` merge: a separately supplied
// tag must not be appended to an image that already names one, or
// `nginx:1.26 -t latest` becomes `nginx:1.26:latest`.
func TestHasReferenceSuffix(t *testing.T) {
	cases := []struct {
		image string
		want  bool
	}{
		{"nginx", false},
		{"nginx:1.26", true},
		{"library/nginx", false},
		{"library/nginx:latest", true},
		{"harbor.abc.com/a/b/nginx", false},
		{"harbor.abc.com/a/b/nginx:1.26", true},
		{"harbor.abc.com:5000/a/b/nginx", false}, // the colon is a PORT
		{"harbor.abc.com:5000/a/b/nginx:1.26", true},
		{"localhost:5000/nginx", false},
		{"nginx@sha256:abc", true}, // digest-pinned: appending a tag would be invalid
		{"harbor.abc.com/a/b/nginx:1.26@sha256:abc", true},
		// Malformed (a tag cannot contain '/'), so there is no tag to see.
		// Documented rather than "fixed": this is what the rule means.
		{"myrepo/a:b/c", false},
	}
	for _, tc := range cases {
		t.Run(tc.image, func(t *testing.T) {
			if got := hasReferenceSuffix(tc.image); got != tc.want {
				t.Errorf("hasReferenceSuffix(%q) = %v, want %v", tc.image, got, tc.want)
			}
		})
	}
}

// TestShortIDMatchesThePrefixLookup keeps the displayed id resolvable: the
// list view prints shortID(id) and the lifecycle verbs accept it back.
func TestShortIDMatchesThePrefixLookup(t *testing.T) {
	full := "0123456789abcdef0123456789abcdef"
	short := shortID(full)
	if short != "0123456789" {
		t.Fatalf("shortID = %q, want the first 10 chars", short)
	}
	if len(short) >= len(full) {
		t.Errorf("shortID did not shorten the id")
	}
	// Short enough ids are returned unchanged rather than truncated to ""
	// or panicking on a slice bound.
	if got := shortID("abc"); got != "abc" {
		t.Errorf("shortID(\"abc\") = %q, want it unchanged", got)
	}
	if got := shortID(""); got != "" {
		t.Errorf("shortID(\"\") = %q, want \"\"", got)
	}
}

// TestTruncateStrIsRuneSafe protects the table rendering: a multi-byte CJK
// name truncated by bytes would produce mojibake in the terminal.
func TestTruncateStrIsRuneSafe(t *testing.T) {
	if got := truncateStr("中文镜像名称很长", 4); got != "中文镜…" {
		t.Errorf("truncateStr = %q, want a whole-rune truncation with an ellipsis", got)
	}
	if got := truncateStr("abc", 10); got != "abc" {
		t.Errorf("truncateStr(short) = %q, want it unchanged", got)
	}
	// Must not panic on a width of 0 or 1.
	for _, n := range []int{0, 1} {
		_ = truncateStr("中文", n)
	}
}

// TestFormatting helpers used by the progress panel.
func TestProgressFormatting(t *testing.T) {
	if got := bar(50, 10); got != "█████░░░░░" {
		t.Errorf("bar(50,10) = %q, want 5 filled / 5 empty", got)
	}
	if got := bar(-10, 4); got != "░░░░" {
		t.Errorf("bar(-10,4) = %q, want clamped to empty", got)
	}
	if got := bar(200, 4); got != "████" {
		t.Errorf("bar(200,4) = %q, want clamped to full", got)
	}
	if got := bar(0, 0); got != "" {
		t.Errorf("bar(0,0) = %q, want empty", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
