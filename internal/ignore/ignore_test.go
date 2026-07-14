// Tests for .dockerignore matching. The reference behavior is Docker's
// patternmatcher: last match wins, `!` re-includes, matched directories
// cover their subtrees. Getting any of these wrong silently changes which
// files buildbust hashes — and therefore which culprit it names.
package ignore

import (
	"strings"
	"testing"
)

func matcher(t *testing.T, lines ...string) *Matcher {
	t.Helper()
	m, err := FromPatterns(lines)
	if err != nil {
		t.Fatalf("FromPatterns: %v", err)
	}
	return m
}

func TestIgnoredBasicPatterns(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{"literal file", "secret.txt", "secret.txt", true},
		{"literal misses sibling", "secret.txt", "other.txt", false},
		{"star within segment", "*.log", "build.log", true},
		{"star must not cross slash", "*.log", "logs/build.log", false},
		{"question mark one char", "v?.json", "v1.json", true},
		{"question mark not two chars", "v?.json", "v12.json", false},
		{"char class", "[ab].txt", "a.txt", true},
		{"nested literal", "docs/notes.md", "docs/notes.md", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := matcher(t, tc.pattern)
			if got := m.Ignored(tc.path); got != tc.want {
				t.Fatalf("Ignored(%q) with %q = %v, want %v", tc.path, tc.pattern, got, tc.want)
			}
		})
	}
}

func TestDoubleStarPatterns(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/*.log", "a.log", true},
		{"**/*.log", "deep/nested/a.log", true},
		{"node_modules/**", "node_modules/pkg/index.js", true},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/c", false},
	}
	for _, tc := range cases {
		m := matcher(t, tc.pattern)
		if got := m.Ignored(tc.path); got != tc.want {
			t.Fatalf("Ignored(%q) with %q = %v, want %v", tc.path, tc.pattern, got, tc.want)
		}
	}
}

func TestDirectoryPatternCoversChildren(t *testing.T) {
	m := matcher(t, "vendor")
	if !m.Ignored("vendor/lib/util.go") {
		t.Fatal("a matched directory must cover everything inside it")
	}
	if m.Ignored("vendored/file.go") {
		t.Fatal("prefix similarity must not match")
	}
}

func TestNegationLastMatchWins(t *testing.T) {
	m := matcher(t, "*.md", "!README.md")
	if !m.Ignored("NOTES.md") {
		t.Fatal("NOTES.md should be ignored")
	}
	if m.Ignored("README.md") {
		t.Fatal("README.md was re-included by the later pattern")
	}
	// Reversed order: the exclusion comes last and wins again.
	m = matcher(t, "!README.md", "*.md")
	if !m.Ignored("README.md") {
		t.Fatal("later *.md must override the earlier negation")
	}
}

func TestNegationReincludesInsideExcludedDir(t *testing.T) {
	m := matcher(t, "docs", "!docs/keep.md")
	if !m.Ignored("docs/drop.md") {
		t.Fatal("docs/drop.md should stay ignored")
	}
	if m.Ignored("docs/keep.md") {
		t.Fatal("docs/keep.md was explicitly re-included")
	}
}

func TestPatternCleaning(t *testing.T) {
	// Leading slash, ./ prefixes, doubled separators, and trailing slashes
	// are all normalized the way Docker normalizes them; comments and
	// blank lines are skipped; a bare "." is a no-op pattern.
	m := matcher(t, "# a comment", "", "   ", "/logs/", "./cache//tmp/", ".", "./")
	pats := m.Patterns()
	if len(pats) != 2 || pats[0] != "logs" || pats[1] != "cache/tmp" {
		t.Fatalf("cleaned = %q", pats)
	}
	if !m.Ignored("logs/x.txt") || !m.Ignored("cache/tmp/y") {
		t.Fatal("cleaned patterns must still match")
	}
}

func TestBadPatternIsError(t *testing.T) {
	if _, err := FromPatterns([]string{"["}); err == nil {
		t.Fatal("want error for malformed character class")
	}
	if _, err := FromPatterns([]string{"!"}); err == nil {
		t.Fatal("want error for bare negation")
	}
}

func TestParseFromReader(t *testing.T) {
	m, err := Parse(strings.NewReader("node_modules\n!node_modules/keep.js\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !m.HasNegations() {
		t.Fatal("negation not detected")
	}
	if got := m.Patterns(); len(got) != 2 || got[1] != "!node_modules/keep.js" {
		t.Fatalf("patterns = %q", got)
	}
	// A missing .dockerignore yields a nil matcher: it must behave inertly.
	var nilM *Matcher
	if nilM.Ignored("anything") || nilM.HasNegations() || nilM.Patterns() != nil {
		t.Fatal("nil matcher must ignore nothing and expose nothing")
	}
}

func TestExportedHelpers(t *testing.T) {
	// Match: exact path matching for COPY/ADD source globs.
	if !Match("src/*.go", "src/main.go") {
		t.Fatal("glob should match")
	}
	if Match("src/*.go", "src/sub/main.go") {
		t.Fatal("single star must not cross a slash")
	}
	// Covers: pattern hits an ancestor directory, subtree is included.
	if !Covers("src/*", "src/app/deep/main.go") {
		t.Fatal("src/* matches the directory src/app, which covers the file")
	}
	if Covers("lib/*", "src/app/main.go") {
		t.Fatal("unrelated tree must not be covered")
	}
	if !Covers("src/main.go", "src/main.go") {
		t.Fatal("exact match counts as covered")
	}
	// Valid: pattern syntax gate used for COPY sources.
	if err := Valid("src/**/*.go"); err != nil {
		t.Fatalf("valid pattern rejected: %v", err)
	}
	if err := Valid("src/[bad"); err == nil {
		t.Fatal("want error for malformed pattern")
	}
}
