// Package cachekey turns a parsed Dockerfile plus a scanned build context
// into a per-instruction cache-key plan — a faithful, offline model of how
// the Docker builder decides "cached" vs "rebuild" for every step.
//
// The model, in one paragraph: every instruction gets a key derived from
// its resolved text; COPY and ADD keys additionally fold in the content
// digests, modes and paths of every context file the instruction would
// pull in; RUN keys fold in the values of all ARGs declared in the stage
// (the builder exports them as environment variables, so a --build-arg
// change misses exactly at the first RUN after the declaration); a stage
// that references another stage (FROM <stage>, COPY --from, RUN
// --mount=from=…) rebuilds from the referencing step when the source stage
// is invalidated. Divergences from BuildKit are documented in
// docs/cache-model.md.
package cachekey

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/JaydenCJ/buildbust/internal/contextscan"
	"github.com/JaydenCJ/buildbust/internal/dockerfile"
	"github.com/JaydenCJ/buildbust/internal/ignore"
)

// IgnoreInfo records which .dockerignore governed the scan, so diffs can
// tell "a file appeared" apart from "your ignore rules changed".
type IgnoreInfo struct {
	Present  bool     `json:"present"`
	Source   string   `json:"source,omitempty"`
	Digest   string   `json:"digest,omitempty"`
	Patterns []string `json:"patterns,omitempty"`
}

// Step is one build step with its computed cache key.
type Step struct {
	// Index is 1-based across the whole build, matching `docker build`
	// step numbering.
	Index      int    `json:"index"`
	Stage      string `json:"stage"`
	StageIndex int    `json:"stage_index"`
	Line       int    `json:"line"`
	Cmd        string `json:"cmd"`
	// Text is the resolved instruction as keyed: variables expanded where
	// the builder expands them, heredoc bodies appended.
	Text string `json:"text"`
	// Key is "sha256:<hex>" over Cmd, Text, FilesDigest and ArgsUsed.
	Key string `json:"key"`
	// Sources are the cleaned COPY/ADD source patterns from the context.
	Sources []string `json:"sources,omitempty"`
	// Files are the context files this COPY/ADD pulls in, sorted by path.
	Files []contextscan.FileEntry `json:"files,omitempty"`
	// FilesDigest is the combined digest of Files.
	FilesDigest string `json:"files_digest,omitempty"`
	// DependsOnStage names the earlier stage this step reads from
	// (FROM <stage>, COPY --from=<stage>, RUN --mount=from=<stage>).
	DependsOnStage string `json:"depends_on_stage,omitempty"`
	// ArgsUsed holds the stage-declared ARG values a RUN step sees.
	ArgsUsed map[string]string `json:"args_used,omitempty"`
	// Notes carry honest caveats ("remote source keyed by URL only", …).
	Notes []string `json:"notes,omitempty"`
}

// Plan is the full keyed build.
type Plan struct {
	BuildArgs map[string]string `json:"build_args,omitempty"`
	Ignore    IgnoreInfo        `json:"ignore"`
	Stages    []string          `json:"stages"`
	Steps     []Step            `json:"steps"`
}

// Compute keys every instruction in df against the scanned context files.
func Compute(df *dockerfile.File, ctx []contextscan.FileEntry, buildArgs map[string]string, ign IgnoreInfo) (*Plan, error) {
	plan := &Plan{BuildArgs: buildArgs, Ignore: ign}
	esc := df.EscapeChar

	// Global (pre-FROM) ARG scope: usable in FROM lines, and re-importable
	// inside stages with a bare `ARG NAME`.
	global := map[string]string{}
	globalLookup := func(name string) (string, bool) {
		v, ok := global[name]
		return v, ok
	}
	for _, in := range df.GlobalArgs {
		kvs, err := dockerfile.ParseArgKeyValues(in.ArgsRaw, esc)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", in.Line, err)
		}
		for _, kv := range kvs {
			v := ""
			if kv.HasValue {
				v, err = dockerfile.Expand(kv.Value, esc, globalLookup)
				if err != nil {
					return nil, fmt.Errorf("line %d: %v", in.Line, err)
				}
			}
			if bv, ok := buildArgs[kv.Key]; ok {
				v = bv
			}
			global[kv.Key] = v
		}
	}

	stageByName := map[string]int{}
	idx := 0
	for si := range df.Stages {
		st := &df.Stages[si]
		label := st.Name
		if label == "" {
			label = "stage-" + strconv.Itoa(si)
		}
		plan.Stages = append(plan.Stages, label)

		// FROM: expanded with the global ARG scope only.
		fromText, err := expandText(st.From, esc, globalLookup)
		if err != nil {
			return nil, err
		}
		idx++
		fstep := Step{Index: idx, Stage: label, StageIndex: si, Line: st.From.Line, Cmd: "FROM", Text: fromText}
		base, err := dockerfile.Expand(st.BaseImage, esc, globalLookup)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", st.From.Line, err)
		}
		if dep, ok := resolveStageRef(base, stageByName, plan.Stages, si); ok {
			fstep.DependsOnStage = dep
		}
		fstep.Key = keyFor(&fstep)
		plan.Steps = append(plan.Steps, fstep)

		env := map[string]string{}
		args := map[string]string{}
		stageLookup := func(name string) (string, bool) {
			if v, ok := env[name]; ok {
				return v, true
			}
			v, ok := args[name]
			return v, ok
		}

		for _, in := range st.Instructions {
			idx++
			step := Step{Index: idx, Stage: label, StageIndex: si, Line: in.Line, Cmd: in.Cmd}
			switch in.Cmd {
			case "ARG":
				// The ARG line itself is keyed on its raw text: passing a
				// different --build-arg does not rewrite the line, it
				// misses at the first RUN (or expansion) that consumes it.
				step.Text = in.Raw
				kvs, err := dockerfile.ParseArgKeyValues(in.ArgsRaw, esc)
				if err != nil {
					return nil, fmt.Errorf("line %d: %v", in.Line, err)
				}
				for _, kv := range kvs {
					v := ""
					switch {
					case kv.HasValue:
						v, err = dockerfile.Expand(kv.Value, esc, stageLookup)
						if err != nil {
							return nil, fmt.Errorf("line %d: %v", in.Line, err)
						}
					default:
						v = global[kv.Key] // bare ARG NAME re-imports the global value
					}
					if bv, ok := buildArgs[kv.Key]; ok {
						v = bv
					}
					args[kv.Key] = v
				}
			case "ENV":
				text, err := expandText(in, esc, stageLookup)
				if err != nil {
					return nil, err
				}
				step.Text = text
				kvs, err := dockerfile.ParseEnvKeyValues(in.ArgsRaw, esc)
				if err != nil {
					return nil, fmt.Errorf("line %d: %v", in.Line, err)
				}
				for _, kv := range kvs {
					v, err := dockerfile.Expand(kv.Value, esc, stageLookup)
					if err != nil {
						return nil, fmt.Errorf("line %d: %v", in.Line, err)
					}
					env[kv.Key] = v
				}
			case "RUN":
				step.Text = in.Raw + heredocSuffix(in, esc, nil)
				if len(args) > 0 {
					step.ArgsUsed = cloneMap(args)
				}
				if from, ok := mountFrom(in); ok {
					ref, err := dockerfile.Expand(from, esc, stageLookup)
					if err != nil {
						return nil, fmt.Errorf("line %d: %v", in.Line, err)
					}
					if dep, ok := resolveStageRef(ref, stageByName, plan.Stages, si); ok {
						step.DependsOnStage = dep
					}
				}
			case "COPY", "ADD":
				if err := keyCopyStep(&step, in, esc, stageLookup, ctx, stageByName, plan.Stages, si); err != nil {
					return nil, err
				}
			default:
				if dockerfile.Expands(in.Cmd) {
					text, err := expandText(in, esc, stageLookup)
					if err != nil {
						return nil, err
					}
					step.Text = text
				} else {
					step.Text = in.Raw
				}
			}
			step.Key = keyFor(&step)
			plan.Steps = append(plan.Steps, step)
		}
		if st.Name != "" {
			stageByName[st.Name] = si
		}
	}
	return plan, nil
}

// keyCopyStep resolves a COPY/ADD instruction: expands its text, selects
// the context files its sources pull in, and records stage dependencies.
func keyCopyStep(step *Step, in dockerfile.Instruction, esc byte, lookup func(string) (string, bool), ctx []contextscan.FileEntry, byName map[string]int, labels []string, current int) error {
	text, err := expandText(in, esc, lookup)
	if err != nil {
		return err
	}
	step.Text = text + heredocSuffix(in, esc, lookup)

	if fromVals, ok := in.Flags["from"]; ok && len(fromVals) > 0 {
		ref, err := dockerfile.Expand(fromVals[len(fromVals)-1], esc, lookup)
		if err != nil {
			return fmt.Errorf("line %d: %v", in.Line, err)
		}
		if dep, ok := resolveStageRef(ref, byName, labels, current); ok {
			step.DependsOnStage = dep
		} else {
			step.Notes = append(step.Notes, fmt.Sprintf("copies from external image %q — contents keyed by reference only (offline)", ref))
		}
		return nil // sources come from the other stage/image, not the context
	}
	if len(in.Heredocs) > 0 {
		return nil // inline heredoc content is the source; already in Text
	}
	if len(in.Args) < 2 {
		return fmt.Errorf("line %d: %s requires at least one source and a destination", in.Line, in.Cmd)
	}

	sources := make([]string, 0, len(in.Args)-1)
	for _, src := range in.Args[:len(in.Args)-1] {
		expanded, err := dockerfile.Expand(src, esc, lookup)
		if err != nil {
			return fmt.Errorf("line %d: %v", in.Line, err)
		}
		if isRemote(expanded) {
			step.Notes = append(step.Notes, fmt.Sprintf("remote source %q keyed by URL only (offline — content not fetched)", expanded))
			continue
		}
		sources = append(sources, cleanSource(expanded))
	}
	var excludes []string
	for _, ex := range in.Flags["exclude"] {
		expanded, err := dockerfile.Expand(ex, esc, lookup)
		if err != nil {
			return fmt.Errorf("line %d: %v", in.Line, err)
		}
		excludes = append(excludes, expanded)
	}
	files, notes, err := resolveSources(sources, excludes, ctx)
	if err != nil {
		return fmt.Errorf("line %d: %v", in.Line, err)
	}
	step.Sources = sources
	step.Files = files
	step.FilesDigest = contextscan.Digest(files)
	step.Notes = append(step.Notes, notes...)
	return nil
}

// resolveSources selects the context files matched by the given cleaned
// source patterns, minus --exclude patterns, deduplicated and sorted.
func resolveSources(sources, excludes []string, ctx []contextscan.FileEntry) ([]contextscan.FileEntry, []string, error) {
	excludeM, err := ignore.FromPatterns(excludes)
	if err != nil {
		return nil, nil, err
	}
	var notes []string
	seen := map[string]bool{}
	var out []contextscan.FileEntry
	for _, src := range sources {
		if strings.HasPrefix(src, "..") {
			notes = append(notes, fmt.Sprintf("source %q escapes the build context (a real build would fail)", src))
			continue
		}
		if err := ignore.Valid(src); err != nil {
			return nil, nil, err
		}
		hasMeta := strings.ContainsAny(src, "*?[")
		matched := false
		for _, f := range ctx {
			rel, ok := sourceMatch(src, hasMeta, f.Path)
			if !ok {
				continue
			}
			matched = true
			if excludeM.Ignored(rel) || excludeM.Ignored(f.Path) {
				continue
			}
			if !seen[f.Path] {
				seen[f.Path] = true
				out = append(out, f)
			}
		}
		if !matched {
			notes = append(notes, fmt.Sprintf("source %q matched no files in the build context", src))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, notes, nil
}

// sourceMatch reports whether the context file at path fp is pulled in by
// the source pattern, and returns fp relative to the matched source root
// (used for --exclude matching).
func sourceMatch(src string, hasMeta bool, fp string) (string, bool) {
	if src == "." {
		return fp, true
	}
	if !hasMeta {
		if fp == src {
			return path.Base(fp), true
		}
		if strings.HasPrefix(fp, src+"/") {
			return fp[len(src)+1:], true
		}
		return "", false
	}
	if ignore.Match(src, fp) {
		return path.Base(fp), true
	}
	if ignore.Covers(src, fp) {
		// The pattern matched an ancestor directory: the whole subtree is
		// copied, e.g. COPY `src/*` pulling in `src/app/main.go`.
		return fp, true
	}
	return "", false
}

func cleanSource(src string) string {
	return path.Clean(strings.TrimPrefix(strings.TrimSpace(src), "/"))
}

func isRemote(src string) bool {
	lower := strings.ToLower(src)
	return strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "git@") ||
		strings.HasPrefix(lower, "git://") ||
		strings.HasPrefix(lower, "ssh://")
}

// mountFrom extracts `from=<ref>` out of RUN --mount flags.
func mountFrom(in dockerfile.Instruction) (string, bool) {
	for _, mount := range in.Flags["mount"] {
		for _, part := range strings.Split(mount, ",") {
			if v, ok := strings.CutPrefix(part, "from="); ok {
				return v, true
			}
		}
	}
	return "", false
}

// resolveStageRef maps a base/--from reference to an earlier stage label.
// Stage names are case-insensitive; bare integers address stages by index.
func resolveStageRef(ref string, byName map[string]int, labels []string, current int) (string, bool) {
	if i, ok := byName[strings.ToLower(ref)]; ok {
		return labels[i], true
	}
	if n, err := strconv.Atoi(ref); err == nil && n >= 0 && n < current {
		return labels[n], true
	}
	return "", false
}

func expandText(in dockerfile.Instruction, esc byte, lookup func(string) (string, bool)) (string, error) {
	expanded, err := dockerfile.Expand(in.ArgsRaw, esc, lookup)
	if err != nil {
		return "", fmt.Errorf("line %d: %v", in.Line, err)
	}
	return in.Cmd + " " + expanded, nil
}

// heredocSuffix folds heredoc bodies into the keyed text, expanding them
// only when the delimiter was unquoted and the instruction expands
// variables (COPY/ADD; RUN bodies are resolved by the shell, not the
// builder). lookup == nil disables expansion.
func heredocSuffix(in dockerfile.Instruction, esc byte, lookup func(string) (string, bool)) string {
	if len(in.Heredocs) == 0 {
		return ""
	}
	var b strings.Builder
	for _, h := range in.Heredocs {
		content := h.Content
		if h.Expand && lookup != nil {
			if expanded, err := dockerfile.Expand(content, esc, lookup); err == nil {
				content = expanded
			}
		}
		b.WriteString("\n<<")
		b.WriteString(h.Name)
		b.WriteString("\n")
		b.WriteString(content)
	}
	return b.String()
}

// keyFor computes the step's cache key from everything the builder would
// look at: instruction kind, resolved text, pulled-in file digests, and
// the ARG environment for RUN steps.
func keyFor(s *Step) string {
	h := sha256.New()
	io.WriteString(h, s.Cmd)
	h.Write([]byte{0})
	io.WriteString(h, s.Text)
	h.Write([]byte{0})
	io.WriteString(h, s.FilesDigest)
	h.Write([]byte{0})
	keys := make([]string, 0, len(s.ArgsUsed))
	for k := range s.ArgsUsed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\x00", k, s.ArgsUsed[k])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
