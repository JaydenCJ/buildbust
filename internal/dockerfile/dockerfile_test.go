// Tests for the Dockerfile parser: continuation joining, parser
// directives, heredocs, JSON/shell forms, flags, and stage structure.
// Each case pins behavior the cache-key model depends on — a misparse
// here would blame the wrong line.
package dockerfile

import (
	"strings"
	"testing"
)

func parse(t *testing.T, src string) *File {
	t.Helper()
	f, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}

func TestParseSimpleDockerfile(t *testing.T) {
	f := parse(t, "FROM alpine:3.20\nRUN apk add curl\nCOPY app /app\nCMD [\"/app\"]\n")
	if len(f.Stages) != 1 {
		t.Fatalf("stages = %d, want 1", len(f.Stages))
	}
	st := f.Stages[0]
	if st.BaseImage != "alpine:3.20" {
		t.Fatalf("base = %q", st.BaseImage)
	}
	if got := len(st.Instructions); got != 3 {
		t.Fatalf("instructions = %d, want 3", got)
	}
	if st.Instructions[0].Cmd != "RUN" || st.Instructions[0].Line != 2 {
		t.Fatalf("first instruction = %+v", st.Instructions[0])
	}
	if st.Instructions[1].ArgsRaw != "app /app" {
		t.Fatalf("COPY args = %q", st.Instructions[1].ArgsRaw)
	}
}

func TestParseLineContinuations(t *testing.T) {
	f := parse(t, "FROM alpine\nRUN apk add \\\n    curl \\\n    git\n")
	run := f.Stages[0].Instructions[0]
	if run.Raw != "RUN apk add     curl     git" {
		t.Fatalf("joined = %q", run.Raw)
	}
	if run.Line != 2 {
		t.Fatalf("line = %d, want 2 (start of the instruction)", run.Line)
	}
}

func TestParseCommentAndBlankInsideContinuation(t *testing.T) {
	// The builder elides comment lines and blank lines inside a continued
	// instruction; a parser that kept them would key a different text.
	f := parse(t, "FROM alpine\nRUN apk add \\\n  # a comment\n\n  curl\n")
	run := f.Stages[0].Instructions[0]
	if run.Raw != "RUN apk add   curl" {
		t.Fatalf("joined = %q", run.Raw)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"duplicate parser directive", "# escape=\\\n# escape=`\nFROM alpine\n"},
		{"unknown instruction", "FROM alpine\nRUNN echo typo\n"},
		{"instruction before FROM", "RUN echo hi\nFROM alpine\n"},
		{"no FROM at all", "# only a comment\n"},
		{"unterminated heredoc", "FROM alpine\nRUN <<EOF\necho hi\n"},
		{"FROM with stray token", "FROM alpine extra\n"},
		{"FROM with wrong keyword", "FROM alpine WITH name\n"},
		{"FROM without image", "FROM\n"},
		{"FROM with too many tokens", "FROM a AS b AS c\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(tc.src)); err == nil {
				t.Fatalf("want parse error for %q", tc.src)
			}
		})
	}
}

func TestParseEscapeDirectiveBacktick(t *testing.T) {
	// Windows-style Dockerfiles switch the escape char so paths like
	// C:\app survive; continuations must then use the backtick.
	f := parse(t, "# escape=`\nFROM alpine\nRUN echo one `\n  two\n")
	if f.EscapeChar != '`' {
		t.Fatalf("escape = %q", f.EscapeChar)
	}
	run := f.Stages[0].Instructions[0]
	if run.Raw != "RUN echo one   two" {
		t.Fatalf("joined = %q", run.Raw)
	}
}

func TestParseDirectivesRecordedAndStopAtFirstInstruction(t *testing.T) {
	f := parse(t, "# syntax=docker/dockerfile:1\n# check=skip=all\nFROM alpine\n# escape=`\nRUN echo hi\n")
	if f.Directives["syntax"] != "docker/dockerfile:1" {
		t.Fatalf("syntax directive = %q", f.Directives["syntax"])
	}
	if f.Directives["check"] != "skip=all" {
		t.Fatalf("check directive = %q", f.Directives["check"])
	}
	// The escape "directive" after FROM is a plain comment: the default
	// escape char must still be backslash.
	if f.EscapeChar != '\\' {
		t.Fatalf("escape = %q, want backslash", f.EscapeChar)
	}
}

func TestParseWindowsCRLFAndBOM(t *testing.T) {
	f := parse(t, "\uFEFFFROM alpine\r\nRUN echo hi\r\n")
	if len(f.Stages) != 1 || f.Stages[0].BaseImage != "alpine" {
		t.Fatalf("stages = %+v", f.Stages)
	}
	if f.Stages[0].Instructions[0].Raw != "RUN echo hi" {
		t.Fatalf("raw = %q", f.Stages[0].Instructions[0].Raw)
	}
}

func TestParseJSONFormCopy(t *testing.T) {
	f := parse(t, "FROM alpine\nCOPY [\"my file.txt\", \"/dest dir/\"]\n")
	cp := f.Stages[0].Instructions[0]
	if !cp.JSONForm {
		t.Fatal("want JSON form")
	}
	if len(cp.Args) != 2 || cp.Args[0] != "my file.txt" {
		t.Fatalf("args = %q", cp.Args)
	}
}

func TestParseMalformedJSONFallsBackToShellForm(t *testing.T) {
	// Docker treats a non-array `[...]` as shell form (it becomes a shell
	// word); the parser must not error out.
	f := parse(t, "FROM alpine\nCMD [not json\n")
	cmd := f.Stages[0].Instructions[0]
	if cmd.JSONForm {
		t.Fatal("want shell form fallback")
	}
	if len(cmd.Args) != 2 || cmd.Args[0] != "[not" {
		t.Fatalf("args = %q", cmd.Args)
	}
}

func TestParseCopyFlags(t *testing.T) {
	f := parse(t, "FROM alpine\nCOPY --from=builder --chown=app:app --exclude=*.md --exclude=*.log src/ /app/\n")
	cp := f.Stages[0].Instructions[0]
	if got := cp.Flags["from"]; len(got) != 1 || got[0] != "builder" {
		t.Fatalf("from flag = %q", got)
	}
	if got := cp.Flags["chown"]; len(got) != 1 || got[0] != "app:app" {
		t.Fatalf("chown flag = %q", got)
	}
	if got := cp.Flags["exclude"]; len(got) != 2 || got[0] != "*.md" || got[1] != "*.log" {
		t.Fatalf("exclude flags = %q", got)
	}
	if cp.ArgsAfterFlags != "src/ /app/" {
		t.Fatalf("args after flags = %q", cp.ArgsAfterFlags)
	}
}

func TestParseRunMountFlag(t *testing.T) {
	f := parse(t, "FROM alpine\nRUN --mount=type=cache,target=/root/.cache go build ./...\n")
	run := f.Stages[0].Instructions[0]
	if got := run.Flags["mount"]; len(got) != 1 || got[0] != "type=cache,target=/root/.cache" {
		t.Fatalf("mount flag = %q", got)
	}
	if run.ArgsAfterFlags != "go build ./..." {
		t.Fatalf("command = %q", run.ArgsAfterFlags)
	}
}

func TestParseHeredocBasic(t *testing.T) {
	f := parse(t, "FROM alpine\nRUN <<EOF\napk add curl\napk add git\nEOF\nCMD run\n")
	st := f.Stages[0]
	if len(st.Instructions) != 2 {
		t.Fatalf("instructions = %d, want 2 (heredoc body must not leak)", len(st.Instructions))
	}
	run := st.Instructions[0]
	if len(run.Heredocs) != 1 {
		t.Fatalf("heredocs = %d", len(run.Heredocs))
	}
	h := run.Heredocs[0]
	if h.Name != "EOF" || h.Content != "apk add curl\napk add git\n" || !h.Expand {
		t.Fatalf("heredoc = %+v", h)
	}
}

func TestParseHeredocVariants(t *testing.T) {
	// Quoted delimiter (<<'EOF') disables expansion in the body.
	f := parse(t, "FROM alpine\nCOPY <<'EOF' /app/config\nvalue=$HOME\nEOF\n")
	h := f.Stages[0].Instructions[0].Heredocs[0]
	if h.Expand {
		t.Fatal("quoted delimiter must disable expansion")
	}
	if h.Content != "value=$HOME\n" {
		t.Fatalf("content = %q", h.Content)
	}
	// The <<- form strips leading tabs from body and delimiter.
	f = parse(t, "FROM alpine\nRUN <<-EOF\n\techo one\n\t\techo two\n\tEOF\n")
	h = f.Stages[0].Instructions[0].Heredocs[0]
	if h.Content != "echo one\necho two\n" {
		t.Fatalf("content = %q", h.Content)
	}
	// `<< EOF` (with a space) is a shell redirect, not a BuildKit heredoc;
	// treating it as one would swallow the next instructions.
	f = parse(t, "FROM alpine\nRUN cat << EOF\nCMD run\n")
	if len(f.Stages[0].Instructions) != 2 {
		t.Fatalf("instructions = %d, want 2", len(f.Stages[0].Instructions))
	}
}

func TestParseMultipleStagesAndNames(t *testing.T) {
	f := parse(t, "FROM golang:1.22 As Build\nRUN go build\nFROM alpine\nCOPY --from=build /bin/app /app\n")
	if len(f.Stages) != 2 {
		t.Fatalf("stages = %d", len(f.Stages))
	}
	if f.Stages[0].Name != "build" {
		t.Fatalf("stage name = %q (must be lower-cased)", f.Stages[0].Name)
	}
	if f.Stages[1].Name != "" || f.Stages[1].Index != 1 {
		t.Fatalf("second stage = %+v", f.Stages[1])
	}
}

func TestParseArgBeforeFromIsGlobal(t *testing.T) {
	f := parse(t, "ARG VERSION=3.20\nFROM alpine:$VERSION\n")
	if len(f.GlobalArgs) != 1 || f.GlobalArgs[0].ArgsRaw != "VERSION=3.20" {
		t.Fatalf("global args = %+v", f.GlobalArgs)
	}
}
