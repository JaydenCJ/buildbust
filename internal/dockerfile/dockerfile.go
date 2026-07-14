// Package dockerfile parses Dockerfiles the way the builder reads them:
// parser directives (`# escape=`), line continuations with comment and
// blank-line elision, heredocs on RUN/COPY/ADD, JSON and shell forms,
// instruction flags, ARG/ENV key-value grammar, and multi-stage structure.
package dockerfile

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Instruction is one logical Dockerfile instruction after continuation
// joining, with flags separated from arguments.
type Instruction struct {
	// Cmd is the canonical upper-case instruction name, e.g. "COPY".
	Cmd string
	// Raw is the full logical line as written (continuations joined,
	// escape-newlines removed), whitespace-trimmed.
	Raw string
	// ArgsRaw is everything after the instruction name, flags included.
	ArgsRaw string
	// ArgsAfterFlags is ArgsRaw with leading --flag tokens removed.
	ArgsAfterFlags string
	// Flags holds parsed leading --name=value options; repeatable flags
	// (COPY --exclude, RUN --mount) accumulate.
	Flags map[string][]string
	// Args is ArgsAfterFlags split into words: the JSON array for JSON
	// form, whitespace fields otherwise.
	Args []string
	// JSONForm is true when the arguments were a valid JSON string array.
	JSONForm bool
	// Heredocs holds inline documents attached to this instruction.
	Heredocs []Heredoc
	// Line is the 1-based line number where the instruction starts.
	Line int
}

// Heredoc is one `<<DELIM … DELIM` block attached to an instruction.
type Heredoc struct {
	Name string
	// Content is the body with a trailing newline; for `<<-` forms the
	// leading tabs are already stripped.
	Content string
	// Expand is false when the delimiter was quoted (<<'EOF' or <<"EOF"),
	// which disables variable expansion inside the body.
	Expand bool
}

// Stage is one FROM-delimited build stage.
type Stage struct {
	Index int
	// Name is the `AS <name>` alias, lower-cased ("" when unnamed).
	Name string
	From Instruction
	// BaseImage is the image reference as written, before expansion.
	BaseImage string
	// Instructions are the stage's instructions after the FROM line.
	Instructions []Instruction
}

// File is a parsed Dockerfile.
type File struct {
	EscapeChar byte
	// Directives holds `# key=value` parser directives from the top of
	// the file (syntax, escape, check, …), keys lower-cased.
	Directives map[string]string
	// GlobalArgs are ARG instructions declared before the first FROM.
	GlobalArgs []Instruction
	Stages     []Stage
}

var knownInstructions = map[string]bool{
	"ADD": true, "ARG": true, "CMD": true, "COPY": true, "ENTRYPOINT": true,
	"ENV": true, "EXPOSE": true, "FROM": true, "HEALTHCHECK": true,
	"LABEL": true, "MAINTAINER": true, "ONBUILD": true, "RUN": true,
	"SHELL": true, "STOPSIGNAL": true, "USER": true, "VOLUME": true,
	"WORKDIR": true,
}

// flaggedInstructions accept leading --name=value options.
var flaggedInstructions = map[string]bool{
	"ADD": true, "COPY": true, "FROM": true, "RUN": true, "HEALTHCHECK": true,
}

// heredocInstructions may carry `<<DELIM` inline documents (BuildKit).
var heredocInstructions = map[string]bool{"RUN": true, "COPY": true, "ADD": true}

var directiveRe = regexp.MustCompile(`^#\s*([a-zA-Z][a-zA-Z0-9]*)\s*=\s*(.*?)\s*$`)

// heredocRe finds `<<` markers: optional `-`, optionally quoted delimiter,
// no space between `<<` and the name (matching BuildKit's grammar, which
// also keeps shell redirects like `<< $f` or `2<<1` from false-matching).
var heredocRe = regexp.MustCompile(`<<-?(?:"([A-Za-z_][A-Za-z0-9_]*)"|'([A-Za-z_][A-Za-z0-9_]*)'|([A-Za-z_][A-Za-z0-9_]*))`)

// Parse reads and parses a Dockerfile.
func Parse(r io.Reader) (*File, error) {
	lines, err := readLines(r)
	if err != nil {
		return nil, err
	}
	f := &File{EscapeChar: '\\', Directives: map[string]string{}}

	// Parser directives: `# key=value` comments at the very top. The block
	// ends at the first line that is not directive-shaped.
	i := 0
	for i < len(lines) {
		m := directiveRe.FindStringSubmatch(lines[i])
		if m == nil {
			break
		}
		key := strings.ToLower(m[1])
		if _, dup := f.Directives[key]; dup {
			return nil, fmt.Errorf("line %d: duplicate parser directive %q", i+1, key)
		}
		f.Directives[key] = m[2]
		if key == "escape" {
			if m[2] != `\` && m[2] != "`" {
				return nil, fmt.Errorf("line %d: invalid escape directive %q (want \\ or `)", i+1, m[2])
			}
			f.EscapeChar = m[2][0]
		}
		i++
	}

	sawFrom := false
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			i++
			continue
		}
		startLine := i + 1
		logical := lines[i]
		i++
		// Join continuations; comment and blank lines inside a continued
		// instruction are elided, exactly as the builder does.
		for hasContinuation(logical, f.EscapeChar) {
			logical = strings.TrimRight(logical, " \t")
			logical = logical[:len(logical)-1]
			for i < len(lines) {
				t := strings.TrimSpace(lines[i])
				if t == "" || strings.HasPrefix(t, "#") {
					i++
					continue
				}
				break
			}
			if i >= len(lines) {
				break
			}
			logical += lines[i]
			i++
		}
		inst, err := parseInstruction(logical, startLine)
		if err != nil {
			return nil, err
		}
		// Consume heredoc bodies following the instruction line.
		if heredocInstructions[inst.Cmd] {
			i, err = consumeHeredocs(&inst, lines, i)
			if err != nil {
				return nil, err
			}
		}
		switch {
		case inst.Cmd == "FROM":
			stage, err := parseFrom(inst, len(f.Stages))
			if err != nil {
				return nil, err
			}
			f.Stages = append(f.Stages, stage)
			sawFrom = true
		case !sawFrom:
			if inst.Cmd != "ARG" {
				return nil, fmt.Errorf("line %d: %s before the first FROM (only ARG may appear here)", inst.Line, inst.Cmd)
			}
			f.GlobalArgs = append(f.GlobalArgs, inst)
		default:
			last := &f.Stages[len(f.Stages)-1]
			last.Instructions = append(last.Instructions, inst)
		}
	}
	if !sawFrom {
		return nil, fmt.Errorf("no FROM instruction: a Dockerfile must contain at least one stage")
	}
	return f, nil
}

func readLines(r io.Reader) ([]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var lines []string
	first := true
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if first {
			line = strings.TrimPrefix(line, "\uFEFF")
			first = false
		}
		lines = append(lines, line)
	}
	return lines, sc.Err()
}

func hasContinuation(line string, escape byte) bool {
	t := strings.TrimRight(line, " \t")
	return len(t) > 0 && t[len(t)-1] == escape
}

func parseInstruction(logical string, line int) (Instruction, error) {
	trimmed := strings.TrimSpace(logical)
	name := trimmed
	rest := ""
	if idx := strings.IndexAny(trimmed, " \t"); idx >= 0 {
		name = trimmed[:idx]
		rest = strings.TrimSpace(trimmed[idx+1:])
	}
	cmd := strings.ToUpper(name)
	if !knownInstructions[cmd] {
		return Instruction{}, fmt.Errorf("line %d: unknown instruction %q", line, name)
	}
	inst := Instruction{
		Cmd:     cmd,
		Raw:     trimmed,
		ArgsRaw: rest,
		Flags:   map[string][]string{},
		Line:    line,
	}
	after := rest
	if flaggedInstructions[cmd] {
		for {
			after = strings.TrimLeft(after, " \t")
			if !strings.HasPrefix(after, "--") {
				break
			}
			token := after
			if idx := strings.IndexAny(after, " \t"); idx >= 0 {
				token = after[:idx]
				after = after[idx+1:]
			} else {
				after = ""
			}
			fname := strings.TrimPrefix(token, "--")
			fval := ""
			if eq := strings.IndexByte(fname, '='); eq >= 0 {
				fval = fname[eq+1:]
				fname = fname[:eq]
			}
			if fname == "" {
				return Instruction{}, fmt.Errorf("line %d: malformed flag %q on %s", line, token, cmd)
			}
			inst.Flags[fname] = append(inst.Flags[fname], fval)
		}
	}
	inst.ArgsAfterFlags = strings.TrimSpace(after)
	if strings.HasPrefix(inst.ArgsAfterFlags, "[") {
		var list []string
		if err := json.Unmarshal([]byte(inst.ArgsAfterFlags), &list); err == nil {
			inst.JSONForm = true
			inst.Args = list
		}
	}
	if !inst.JSONForm {
		inst.Args = strings.Fields(inst.ArgsAfterFlags)
	}
	return inst, nil
}

// consumeHeredocs scans the instruction arguments for `<<DELIM` markers and
// consumes the following physical lines as their bodies. Returns the new
// line cursor.
func consumeHeredocs(inst *Instruction, lines []string, i int) (int, error) {
	matches := heredocRe.FindAllStringSubmatch(inst.ArgsAfterFlags, -1)
	for _, m := range matches {
		name := m[1] + m[2] + m[3] // exactly one group is non-empty
		expand := m[3] != ""       // unquoted delimiter → body expands
		strip := strings.HasPrefix(m[0], "<<-")
		var body []string
		closed := false
		for i < len(lines) {
			l := lines[i]
			cmp := l
			if strip {
				cmp = strings.TrimLeft(l, "\t")
			}
			i++
			if cmp == name {
				closed = true
				break
			}
			if strip {
				l = strings.TrimLeft(l, "\t")
			}
			body = append(body, l)
		}
		if !closed {
			return i, fmt.Errorf("line %d: unterminated heredoc %q on %s", inst.Line, name, inst.Cmd)
		}
		inst.Heredocs = append(inst.Heredocs, Heredoc{
			Name:    name,
			Content: strings.Join(body, "\n") + "\n",
			Expand:  expand,
		})
	}
	return i, nil
}

func parseFrom(inst Instruction, index int) (Stage, error) {
	stage := Stage{Index: index, From: inst}
	args := inst.Args
	switch len(args) {
	case 1:
		stage.BaseImage = args[0]
	case 3:
		if !strings.EqualFold(args[1], "AS") {
			return stage, fmt.Errorf("line %d: FROM: expected AS, got %q", inst.Line, args[1])
		}
		stage.BaseImage = args[0]
		stage.Name = strings.ToLower(args[2])
	default:
		return stage, fmt.Errorf("line %d: FROM requires <image> or <image> AS <name>", inst.Line)
	}
	return stage, nil
}
