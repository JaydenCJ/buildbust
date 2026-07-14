package dockerfile

import (
	"fmt"
	"strings"
)

// expandsArgs lists the instructions whose arguments the builder itself
// expands. RUN/CMD/ENTRYPOINT/SHELL/HEALTHCHECK are deliberately absent:
// their variables are resolved by the container shell at run time, so the
// instruction text in the cache key stays as written (ARG values reach RUN
// cache keys through the environment instead — see internal/cachekey).
var expandsArgs = map[string]bool{
	"ADD": true, "ARG": true, "COPY": true, "ENV": true, "EXPOSE": true,
	"FROM": true, "LABEL": true, "STOPSIGNAL": true, "USER": true,
	"VOLUME": true, "WORKDIR": true,
}

// Expands reports whether the builder performs variable expansion on the
// given instruction's arguments.
func Expands(cmd string) bool { return expandsArgs[cmd] }

// Expand substitutes build-time variables in s using Docker's grammar:
// $NAME, ${NAME}, ${NAME:-default} (default when unset or empty) and
// ${NAME:+alternate} (alternate when set and non-empty). The escape
// character before `$` yields a literal dollar sign. Unset variables
// without a modifier expand to the empty string. Default/alternate words
// are themselves expanded, so ${A:-${B}} works.
func Expand(s string, escape byte, lookup func(string) (string, bool)) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c == escape && i+1 < len(s) && s[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(s) {
			b.WriteByte('$')
			i++
			continue
		}
		if s[i+1] == '{' {
			end := findBraceEnd(s, i+2)
			if end < 0 {
				return "", fmt.Errorf("unterminated variable expansion in %q", s)
			}
			val, err := evalBrace(s[i+2:end], escape, lookup)
			if err != nil {
				return "", err
			}
			b.WriteString(val)
			i = end + 1
			continue
		}
		name := scanName(s[i+1:])
		if name == "" {
			b.WriteByte('$')
			i++
			continue
		}
		val, _ := lookup(name)
		b.WriteString(val)
		i += 1 + len(name)
	}
	return b.String(), nil
}

// findBraceEnd locates the `}` closing the expansion that starts at from,
// tolerating one level of nesting for ${A:-${B}} style defaults.
func findBraceEnd(s string, from int) int {
	depth := 1
	for i := from; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func scanName(s string) string {
	if len(s) == 0 || !isNameStart(s[0]) {
		return ""
	}
	i := 1
	for i < len(s) && isNameByte(s[i]) {
		i++
	}
	return s[:i]
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameByte(c byte) bool { return isNameStart(c) || (c >= '0' && c <= '9') }

func evalBrace(expr string, escape byte, lookup func(string) (string, bool)) (string, error) {
	name := scanName(expr)
	if name == "" {
		return "", fmt.Errorf("invalid variable expansion ${%s}", expr)
	}
	rest := expr[len(name):]
	val, set := lookup(name)
	if rest == "" {
		return val, nil
	}
	op := rest
	if strings.HasPrefix(rest, ":") {
		op = rest[1:]
	}
	if op == "" {
		return "", fmt.Errorf("invalid variable expansion ${%s}", expr)
	}
	word := op[1:]
	switch op[0] {
	case '-':
		// Default: used when the variable is unset or empty.
		if !set || val == "" {
			return Expand(word, escape, lookup)
		}
		return val, nil
	case '+':
		// Alternate: used when the variable is set and non-empty.
		if set && val != "" {
			return Expand(word, escape, lookup)
		}
		return "", nil
	default:
		return "", fmt.Errorf("unsupported modifier %q in ${%s}", string(op[0]), expr)
	}
}
