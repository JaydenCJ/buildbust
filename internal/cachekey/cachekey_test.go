// Tests for the cache-key model: COPY/ADD file selection, ARG scoping and
// its effect on RUN keys, stage dependencies, and key determinism. These
// pin the exact semantics documented in docs/cache-model.md.
package cachekey

import (
	"strings"
	"testing"

	"github.com/JaydenCJ/buildbust/internal/contextscan"
	"github.com/JaydenCJ/buildbust/internal/dockerfile"
)

// ctxFiles builds a fake scanned context; digests only need to be unique
// per content here, so the content string doubles as the digest.
func ctxFiles(pathsToContent map[string]string) []contextscan.FileEntry {
	keys := make([]string, 0, len(pathsToContent))
	for p := range pathsToContent {
		keys = append(keys, p)
	}
	// Deterministic order for stable FilesDigest values across runs.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	out := make([]contextscan.FileEntry, 0, len(keys))
	for _, p := range keys {
		out = append(out, contextscan.FileEntry{
			Path: p, Size: int64(len(pathsToContent[p])), Mode: "0644",
			Digest: "sha256:" + pathsToContent[p],
		})
	}
	return out
}

func compute(t *testing.T, src string, ctx []contextscan.FileEntry, buildArgs map[string]string) *Plan {
	t.Helper()
	df, err := dockerfile.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := Compute(df, ctx, buildArgs, IgnoreInfo{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	return plan
}

func stepByLine(t *testing.T, plan *Plan, line int) Step {
	t.Helper()
	for _, s := range plan.Steps {
		if s.Line == line {
			return s
		}
	}
	t.Fatalf("no step at line %d in %+v", line, plan.Steps)
	return Step{}
}

func filePaths(s Step) []string {
	out := make([]string, len(s.Files))
	for i, f := range s.Files {
		out[i] = f.Path
	}
	return out
}

func TestComputeStepsAndStageLabels(t *testing.T) {
	plan := compute(t, "FROM golang:1.22 AS build\nRUN go build\nFROM alpine\nCOPY --from=build /bin/a /a\n", nil, nil)
	if len(plan.Steps) != 4 {
		t.Fatalf("steps = %d", len(plan.Steps))
	}
	if plan.Stages[0] != "build" || plan.Stages[1] != "stage-1" {
		t.Fatalf("stages = %v", plan.Stages)
	}
	for i, s := range plan.Steps {
		if s.Index != i+1 {
			t.Fatalf("index of step %d = %d (want docker-style 1-based)", i, s.Index)
		}
	}
	if plan.Steps[3].DependsOnStage != "build" {
		t.Fatalf("COPY --from dep = %q", plan.Steps[3].DependsOnStage)
	}
	// Numeric --from references address stages by index.
	byIndex := compute(t, "FROM golang\nRUN go build\nFROM alpine\nCOPY --from=0 /bin/a /a\n", nil, nil)
	if got := stepByLine(t, byIndex, 4).DependsOnStage; got != "stage-0" {
		t.Fatalf("numeric --from dep = %q", got)
	}
}

func TestCopyLiteralFileSelection(t *testing.T) {
	ctx := ctxFiles(map[string]string{"package.json": "a", "package-lock.json": "b", "src/main.js": "c"})
	plan := compute(t, "FROM node\nCOPY package.json package-lock.json ./\n", ctx, nil)
	cp := stepByLine(t, plan, 2)
	got := filePaths(cp)
	if len(got) != 2 || got[0] != "package-lock.json" || got[1] != "package.json" {
		t.Fatalf("files = %v", got)
	}
	if cp.FilesDigest == "" {
		t.Fatal("files digest missing")
	}
}

func TestCopyDirectoryPullsSubtree(t *testing.T) {
	ctx := ctxFiles(map[string]string{"src/a.js": "a", "src/lib/b.js": "b", "test/c.js": "c"})
	plan := compute(t, "FROM node\nCOPY src/ ./src/\n", ctx, nil)
	got := filePaths(stepByLine(t, plan, 2))
	if len(got) != 2 || got[0] != "src/a.js" || got[1] != "src/lib/b.js" {
		t.Fatalf("files = %v", got)
	}
	// `COPY . dest` pulls the whole context.
	plan = compute(t, "FROM node\nCOPY . /app\n", ctx, nil)
	if got := filePaths(stepByLine(t, plan, 2)); len(got) != 3 {
		t.Fatalf("files = %v", got)
	}
}

func TestCopyGlobSelection(t *testing.T) {
	ctx := ctxFiles(map[string]string{"go.mod": "m", "go.sum": "s", "main.go": "x"})
	plan := compute(t, "FROM golang\nCOPY go.* ./\n", ctx, nil)
	got := filePaths(stepByLine(t, plan, 2))
	if len(got) != 2 || got[0] != "go.mod" || got[1] != "go.sum" {
		t.Fatalf("files = %v", got)
	}
	// COPY src/* pulls in whole directories that match the glob, exactly
	// like the builder expands wildcard sources.
	ctx = ctxFiles(map[string]string{"src/app/main.go": "m", "src/top.go": "t", "other/x.go": "o"})
	plan = compute(t, "FROM golang\nCOPY src/* /app/\n", ctx, nil)
	got = filePaths(stepByLine(t, plan, 2))
	if len(got) != 2 || got[0] != "src/app/main.go" || got[1] != "src/top.go" {
		t.Fatalf("files = %v", got)
	}
}

func TestCopyExcludeFlagNarrowsSelection(t *testing.T) {
	ctx := ctxFiles(map[string]string{"src/a.go": "a", "src/notes.md": "n"})
	plan := compute(t, "FROM golang\nCOPY --exclude=*.md src/ /app/\n", ctx, nil)
	got := filePaths(stepByLine(t, plan, 2))
	if len(got) != 1 || got[0] != "src/a.go" {
		t.Fatalf("files = %v", got)
	}
}

func TestCopyNotesAreHonestAboutLimits(t *testing.T) {
	// Sources that cannot be hashed get an explicit note instead of a
	// silently wrong key: missing files, context escapes, external images
	// and remote URLs (buildbust is offline by design).
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"missing source", "FROM alpine\nCOPY absent.txt /x\n", "matched no files"},
		{"context escape", "FROM alpine\nCOPY ../outside /x\n", "escapes the build context"},
		{"external image", "FROM alpine\nCOPY --from=nginx:alpine /etc/nginx /etc/nginx\n", "external image"},
		{"remote URL", "FROM alpine\nADD https://example.test/tool.tar.gz /opt/\n", "keyed by URL only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := compute(t, tc.src, nil, nil)
			cp := stepByLine(t, plan, 2)
			if len(cp.Notes) != 1 || !strings.Contains(cp.Notes[0], tc.want) {
				t.Fatalf("notes = %v, want a note containing %q", cp.Notes, tc.want)
			}
			if cp.DependsOnStage != "" || len(cp.Files) != 0 {
				t.Fatalf("step must carry no stage dep and no hashed files: %+v", cp)
			}
		})
	}
}

func TestRunKeyIncludesStageArgsButArgLineDoesNot(t *testing.T) {
	src := "FROM alpine\nARG VER=1\nRUN echo $VER\n"
	base := compute(t, src, nil, nil)
	bumped := compute(t, src, nil, map[string]string{"VER": "2"})
	// The ARG line's own key is text-only: a --build-arg does not touch it.
	if stepByLine(t, base, 2).Key != stepByLine(t, bumped, 2).Key {
		t.Fatal("ARG line key must not change with --build-arg")
	}
	// The first RUN after the declaration is where the miss lands.
	if stepByLine(t, base, 3).Key == stepByLine(t, bumped, 3).Key {
		t.Fatal("RUN key must change when a consumed ARG changes")
	}
	if got := stepByLine(t, bumped, 3).ArgsUsed["VER"]; got != "2" {
		t.Fatalf("ArgsUsed[VER] = %q", got)
	}
}

func TestGlobalArgNeedsRedeclarationInsideStage(t *testing.T) {
	// A pre-FROM ARG is visible to FROM lines but not inside a stage until
	// re-imported with a bare `ARG NAME` — Docker's documented scoping.
	src := "ARG TAG=3.20\nFROM alpine:$TAG\nCOPY dir-$TAG /x\nARG TAG\nCOPY dir-$TAG /y\n"
	ctx := ctxFiles(map[string]string{"dir-3.20/f": "z", "dir-/f": "w"})
	plan := compute(t, src, ctx, nil)
	if got := stepByLine(t, plan, 2).Text; got != "FROM alpine:3.20" {
		t.Fatalf("FROM text = %q", got)
	}
	if got := stepByLine(t, plan, 3).Text; got != "COPY dir- /x" {
		t.Fatalf("pre-redeclare COPY text = %q (global ARG must not leak in)", got)
	}
	if got := stepByLine(t, plan, 5).Text; got != "COPY dir-3.20 /y" {
		t.Fatalf("post-redeclare COPY text = %q", got)
	}
}

func TestExpansionScoping(t *testing.T) {
	// ENV shadows ARG in expansion, matching the builder's precedence.
	plan := compute(t, "FROM alpine\nARG DIR=fromarg\nENV DIR=fromenv\nWORKDIR /$DIR\n", nil, nil)
	if got := stepByLine(t, plan, 4).Text; got != "WORKDIR /fromenv" {
		t.Fatalf("WORKDIR text = %q (ENV must shadow ARG)", got)
	}
	// The builder does not expand RUN arguments itself — the shell does.
	// The ARG value reaches the key through ArgsUsed instead.
	plan = compute(t, "FROM alpine\nARG V=1\nRUN echo $V\n", nil, nil)
	run := stepByLine(t, plan, 3)
	if run.Text != "RUN echo $V" {
		t.Fatalf("RUN text = %q", run.Text)
	}
	if run.ArgsUsed["V"] != "1" {
		t.Fatalf("ArgsUsed = %v", run.ArgsUsed)
	}
}

func TestRunMountFromCreatesStageDependency(t *testing.T) {
	src := "FROM golang AS deps\nRUN go mod download\nFROM golang\nRUN --mount=type=bind,from=deps,source=/go,target=/go go build\n"
	plan := compute(t, src, nil, nil)
	if got := stepByLine(t, plan, 4).DependsOnStage; got != "deps" {
		t.Fatalf("dep = %q", got)
	}
}

func TestHeredocKeys(t *testing.T) {
	// A heredoc body edit must change the step key even though the
	// instruction line itself is unchanged.
	a := compute(t, "FROM alpine\nRUN <<EOF\necho one\nEOF\n", nil, nil)
	b := compute(t, "FROM alpine\nRUN <<EOF\necho two\nEOF\n", nil, nil)
	if stepByLine(t, a, 2).Key == stepByLine(t, b, 2).Key {
		t.Fatal("heredoc body change must change the key")
	}
	// COPY heredoc bodies are expanded by the builder (unquoted delimiter).
	plan := compute(t, "FROM alpine\nENV PORT=8080\nCOPY <<EOF /etc/app.conf\nlisten $PORT\nEOF\n", nil, nil)
	if cp := stepByLine(t, plan, 3); !strings.Contains(cp.Text, "listen 8080") {
		t.Fatalf("heredoc not expanded: %q", cp.Text)
	}
}

func TestBuildArgOverridesDefaultInFrom(t *testing.T) {
	plan := compute(t, "ARG TAG=3.19\nFROM alpine:$TAG\n", nil, map[string]string{"TAG": "3.20"})
	if got := stepByLine(t, plan, 2).Text; got != "FROM alpine:3.20" {
		t.Fatalf("FROM text = %q", got)
	}
}

func TestKeysAreDeterministic(t *testing.T) {
	src := "FROM alpine\nARG A=1 B=2\nRUN build\nCOPY . /app\n"
	ctx := ctxFiles(map[string]string{"x": "1", "y": "2"})
	a := compute(t, src, ctx, nil)
	b := compute(t, src, ctx, nil)
	for i := range a.Steps {
		if a.Steps[i].Key != b.Steps[i].Key {
			t.Fatalf("step %d key differs across identical runs", i)
		}
	}
}

func TestFilesChangeOnlyAffectsCopyKey(t *testing.T) {
	src := "FROM alpine\nRUN prep\nCOPY data/ /data\nRUN post\n"
	before := compute(t, src, ctxFiles(map[string]string{"data/a": "1"}), nil)
	after := compute(t, src, ctxFiles(map[string]string{"data/a": "2"}), nil)
	if stepByLine(t, before, 2).Key != stepByLine(t, after, 2).Key {
		t.Fatal("RUN before the COPY must keep its key")
	}
	if stepByLine(t, before, 3).Key == stepByLine(t, after, 3).Key {
		t.Fatal("COPY key must change with file content")
	}
	if stepByLine(t, before, 4).Key != stepByLine(t, after, 4).Key {
		t.Fatal("the later RUN's own key is unchanged — invalidation is the diff layer's job")
	}
}
