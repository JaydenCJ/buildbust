// Tests for variable expansion and ARG/ENV word grammar. These decide
// which values land in a cache key, so every Docker-documented form gets
// a pinned case.
package dockerfile

import "testing"

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

func TestExpandForms(t *testing.T) {
	vars := map[string]string{"NAME": "app", "EMPTY": "", "VER": "1.2"}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare variable", "bin/$NAME", "bin/app"},
		{"braced variable", "bin/${NAME}.tar", "bin/app.tar"},
		{"adjacent text needs braces", "${NAME}s", "apps"},
		{"unset expands to empty", "x$MISSINGx", "x"},
		{"default used when unset", "${MISSING:-fallback}", "fallback"},
		{"default used when empty", "${EMPTY:-fallback}", "fallback"},
		{"default skipped when set", "${NAME:-fallback}", "app"},
		{"alternate used when set", "${NAME:+yes}", "yes"},
		{"alternate skipped when unset", "${MISSING:+yes}", ""},
		{"alternate skipped when empty", "${EMPTY:+yes}", ""},
		{"nested default expands", "${MISSING:-${VER}}", "1.2"},
		{"dollar at end is literal", "cost$", "cost$"},
		{"lone dollar digit is literal", "a$1b", "a$1b"},
		{"two variables", "$NAME-$VER", "app-1.2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Expand(tc.in, '\\', lookupFrom(vars))
			if err != nil {
				t.Fatalf("Expand(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("Expand(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExpandEscapedDollar(t *testing.T) {
	got, err := Expand(`price \$HOME`, '\\', lookupFrom(map[string]string{"HOME": "/root"}))
	if err != nil {
		t.Fatal(err)
	}
	if got != "price $HOME" {
		t.Fatalf("got %q", got)
	}
	// With the backtick escape directive, the backtick escapes instead.
	got, err = Expand("price `$HOME", '`', lookupFrom(map[string]string{"HOME": "/root"}))
	if err != nil {
		t.Fatal(err)
	}
	if got != "price $HOME" {
		t.Fatalf("backtick escape: got %q", got)
	}
}

func TestExpandErrors(t *testing.T) {
	if _, err := Expand("${NAME", '\\', lookupFrom(nil)); err == nil {
		t.Fatal("want error for unterminated ${")
	}
	// BuildKit-only string manipulation (${V#prefix} etc.) is out of scope
	// in 0.1.0; failing loudly beats keying the wrong text.
	if _, err := Expand("${NAME#pre}", '\\', lookupFrom(map[string]string{"NAME": "x"})); err == nil {
		t.Fatal("want error for unsupported modifier")
	}
}

func TestSplitWordsQuoting(t *testing.T) {
	words, err := SplitWords(`a "b c" 'd e' f\ g`, '\\')
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b c", "d e", "f g"}
	if len(words) != len(want) {
		t.Fatalf("words = %q", words)
	}
	for i := range want {
		if words[i] != want[i] {
			t.Fatalf("words[%d] = %q, want %q", i, words[i], want[i])
		}
	}
}

func TestWordGrammarErrors(t *testing.T) {
	if _, err := SplitWords(`a "unclosed`, '\\'); err == nil {
		t.Fatal("want error for unterminated quote")
	}
	if _, err := ParseEnvKeyValues("A=1 loose", '\\'); err == nil {
		t.Fatal("want error for bare word in k=v ENV")
	}
}

func TestParseArgKeyValues(t *testing.T) {
	kvs, err := ParseArgKeyValues("VERSION=1.2 BARE OTHER=x=y", '\\')
	if err != nil {
		t.Fatal(err)
	}
	if len(kvs) != 3 {
		t.Fatalf("kvs = %+v", kvs)
	}
	if kvs[0].Key != "VERSION" || kvs[0].Value != "1.2" || !kvs[0].HasValue {
		t.Fatalf("kvs[0] = %+v", kvs[0])
	}
	if kvs[1].Key != "BARE" || kvs[1].HasValue {
		t.Fatalf("kvs[1] = %+v", kvs[1])
	}
	// Only the first '=' splits: values may themselves contain '='.
	if kvs[2].Key != "OTHER" || kvs[2].Value != "x=y" {
		t.Fatalf("kvs[2] = %+v", kvs[2])
	}
}

func TestParseEnvKeyValuesModernForm(t *testing.T) {
	kvs, err := ParseEnvKeyValues(`A=1 B="two words"`, '\\')
	if err != nil {
		t.Fatal(err)
	}
	if len(kvs) != 2 || kvs[1].Key != "B" || kvs[1].Value != "two words" {
		t.Fatalf("kvs = %+v", kvs)
	}
}

func TestParseEnvKeyValuesLegacySpaceForm(t *testing.T) {
	kvs, err := ParseEnvKeyValues("PATH /usr/local/bin:/usr/bin", '\\')
	if err != nil {
		t.Fatal(err)
	}
	if len(kvs) != 1 || kvs[0].Key != "PATH" || kvs[0].Value != "/usr/local/bin:/usr/bin" {
		t.Fatalf("kvs = %+v", kvs)
	}
}
