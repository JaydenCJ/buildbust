package dockerfile

import (
	"fmt"
	"strings"
)

// KeyValue is one `key=value` (or bare `key`) from an ARG/ENV instruction.
type KeyValue struct {
	Key      string
	Value    string
	HasValue bool
}

// SplitWords splits an instruction argument string into words the way the
// builder's word scanner does: whitespace separates words, single and
// double quotes group, and the escape character escapes the next byte
// (except inside single quotes, where everything is literal).
func SplitWords(s string, escape byte) ([]string, error) {
	var words []string
	var b strings.Builder
	inWord := false
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
				continue
			}
			b.WriteByte(c)
		case quote == '"':
			if c == '"' {
				quote = 0
				continue
			}
			if c == escape && i+1 < len(s) {
				i++
				b.WriteByte(s[i])
				continue
			}
			b.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
			inWord = true
		case c == escape && i+1 < len(s):
			i++
			b.WriteByte(s[i])
			inWord = true
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, b.String())
				b.Reset()
				inWord = false
			}
		default:
			b.WriteByte(c)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote %q in %q", string(quote), s)
	}
	if inWord {
		words = append(words, b.String())
	}
	return words, nil
}

// ParseArgKeyValues parses an ARG argument list: each word is either
// `NAME=default` or a bare `NAME` (which re-imports a global ARG).
func ParseArgKeyValues(s string, escape byte) ([]KeyValue, error) {
	words, err := SplitWords(s, escape)
	if err != nil {
		return nil, err
	}
	if len(words) == 0 {
		return nil, fmt.Errorf("ARG requires at least one argument")
	}
	kvs := make([]KeyValue, 0, len(words))
	for _, w := range words {
		if eq := strings.IndexByte(w, '='); eq >= 0 {
			if eq == 0 {
				return nil, fmt.Errorf("ARG: empty variable name in %q", w)
			}
			kvs = append(kvs, KeyValue{Key: w[:eq], Value: w[eq+1:], HasValue: true})
			continue
		}
		kvs = append(kvs, KeyValue{Key: w})
	}
	return kvs, nil
}

// ParseEnvKeyValues parses an ENV argument list. Both forms are accepted:
// the modern `ENV k=v k2=v2` and the legacy `ENV key value with spaces`
// (detected by the first word carrying no `=`).
func ParseEnvKeyValues(s string, escape byte) ([]KeyValue, error) {
	words, err := SplitWords(s, escape)
	if err != nil {
		return nil, err
	}
	if len(words) == 0 {
		return nil, fmt.Errorf("ENV requires at least one argument")
	}
	if !strings.Contains(words[0], "=") {
		// Legacy space form: single key, remainder is the value.
		return []KeyValue{{Key: words[0], Value: strings.Join(words[1:], " "), HasValue: true}}, nil
	}
	kvs := make([]KeyValue, 0, len(words))
	for _, w := range words {
		eq := strings.IndexByte(w, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("ENV: %q is not in key=value form", w)
		}
		kvs = append(kvs, KeyValue{Key: w[:eq], Value: w[eq+1:], HasValue: true})
	}
	return kvs, nil
}
