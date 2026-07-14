// Package ignore implements .dockerignore pattern matching with Docker's
// semantics: patterns are matched in order and the last match wins, `!`
// re-includes previously excluded paths, `*` and `?` match within one path
// segment, `**` spans any number of segments, and a pattern that matches a
// directory covers everything inside it.
package ignore

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"strings"
)

// Pattern is one cleaned .dockerignore rule.
type Pattern struct {
	// Raw is the cleaned pattern text, without the leading "!".
	Raw     string
	Negated bool
	segs    []string
}

// Matcher evaluates an ordered list of .dockerignore patterns.
// The zero value (and a nil *Matcher) ignores nothing.
type Matcher struct {
	patterns    []Pattern
	hasNegation bool
}

// Parse reads .dockerignore content from r.
func Parse(r io.Reader) (*Matcher, error) {
	var lines []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return FromPatterns(lines)
}

// FromPatterns builds a Matcher from raw pattern lines. Blank lines and
// `#` comments are skipped; every kept pattern is cleaned the way Docker
// cleans it (leading "/" stripped, "." and ".." segments resolved).
func FromPatterns(lines []string) (*Matcher, error) {
	m := &Matcher{}
	for i, line := range lines {
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		negated := strings.HasPrefix(text, "!")
		if negated {
			text = strings.TrimSpace(text[1:])
			if text == "" {
				return nil, fmt.Errorf("dockerignore line %d: %q is not a valid pattern", i+1, line)
			}
		}
		cleaned := path.Clean(strings.TrimPrefix(text, "/"))
		if cleaned == "." {
			// A bare "." (or "./") matches the context root itself, which
			// Docker treats as a no-op pattern.
			continue
		}
		p := Pattern{Raw: cleaned, Negated: negated, segs: strings.Split(cleaned, "/")}
		for _, seg := range p.segs {
			if seg == "**" {
				continue
			}
			if _, err := path.Match(seg, "probe"); err != nil {
				return nil, fmt.Errorf("dockerignore line %d: bad pattern %q: %v", i+1, text, err)
			}
		}
		if negated {
			m.hasNegation = true
		}
		m.patterns = append(m.patterns, p)
	}
	return m, nil
}

// Ignored reports whether the slash-separated, context-relative path is
// excluded from the build context. Matching follows Docker: every pattern
// is evaluated in order against the path and each of its ancestor
// directories, and the last pattern that matches decides.
func (m *Matcher) Ignored(p string) bool {
	if m == nil || len(m.patterns) == 0 {
		return false
	}
	segs := splitPath(p)
	ignored := false
	for _, pat := range m.patterns {
		if matchWithAncestors(pat.segs, segs) {
			ignored = !pat.Negated
		}
	}
	return ignored
}

// HasNegations reports whether any `!` pattern is present. Callers use it
// to decide whether an ignored directory can be pruned wholesale: with
// negations in play, children may be re-included and the walk must descend.
func (m *Matcher) HasNegations() bool {
	return m != nil && m.hasNegation
}

// Patterns returns the cleaned pattern list with `!` prefixes restored,
// in evaluation order. Snapshots store this for change reporting.
func (m *Matcher) Patterns() []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.patterns))
	for _, p := range m.patterns {
		if p.Negated {
			out = append(out, "!"+p.Raw)
			continue
		}
		out = append(out, p.Raw)
	}
	return out
}

// Match reports whether a Docker-style pattern matches the path exactly
// (no ancestor coverage). Used for COPY/ADD source globs.
func Match(pattern, p string) bool {
	return matchSegments(splitPattern(pattern), splitPath(p))
}

// Covers reports whether the pattern matches the path itself or any of
// its ancestor directories — that is, whether the path lives inside a
// directory the pattern selects. COPY `src/*` covers `src/app/main.go`
// because the directory `src/app` matches.
func Covers(pattern, p string) bool {
	return matchWithAncestors(splitPattern(pattern), splitPath(p))
}

// Valid reports whether a pattern is syntactically usable.
func Valid(pattern string) error {
	for _, seg := range splitPattern(pattern) {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, "probe"); err != nil {
			return fmt.Errorf("bad pattern %q: %v", pattern, err)
		}
	}
	return nil
}

func splitPattern(pattern string) []string {
	cleaned := path.Clean(strings.TrimPrefix(strings.TrimSpace(pattern), "/"))
	return strings.Split(cleaned, "/")
}

func splitPath(p string) []string {
	cleaned := path.Clean(strings.TrimPrefix(p, "/"))
	return strings.Split(cleaned, "/")
}

// matchWithAncestors reports whether the pattern matches the path or any
// proper ancestor of it (segs[:1] … segs[:len]).
func matchWithAncestors(pat, segs []string) bool {
	for i := 1; i <= len(segs); i++ {
		if matchSegments(pat, segs[:i]) {
			return true
		}
	}
	return false
}

// matchSegments matches pattern segments against path segments. `**`
// consumes zero or more whole segments; every other segment is matched
// with path.Match semantics (`*`, `?`, `[...]` within one segment).
func matchSegments(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		if len(pat) == 1 {
			return true
		}
		for i := 0; i <= len(segs); i++ {
			if matchSegments(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], segs[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pat[1:], segs[1:])
}
