// Tests for the comparison engine. Plans are built through the real
// pipeline (parser → scanner-shaped entries → cachekey), so these are
// end-to-end over the pure core: change one thing, assert buildbust blames
// exactly that thing and draws the right blast radius.
package diff

import (
	"strings"
	"testing"
	"time"

	"github.com/JaydenCJ/buildbust/internal/cachekey"
	"github.com/JaydenCJ/buildbust/internal/contextscan"
	"github.com/JaydenCJ/buildbust/internal/dockerfile"
	"github.com/JaydenCJ/buildbust/internal/snapshot"
)

type fixture struct {
	dockerfile string
	files      map[string]fileSpec
	buildArgs  map[string]string
	ignore     cachekey.IgnoreInfo
}

type fileSpec struct {
	content string
	mode    string
}

func plain(content string) fileSpec { return fileSpec{content: content, mode: "0644"} }

func (fx fixture) plan(t *testing.T) *cachekey.Plan {
	t.Helper()
	df, err := dockerfile.Parse(strings.NewReader(fx.dockerfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	names := make([]string, 0, len(fx.files))
	for p := range fx.files {
		names = append(names, p)
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	var entries []contextscan.FileEntry
	for _, p := range names {
		spec := fx.files[p]
		entries = append(entries, contextscan.FileEntry{
			Path: p, Size: int64(len(spec.content)), Mode: spec.mode,
			Digest: "sha256:" + spec.content,
		})
	}
	plan, err := cachekey.Compute(df, entries, fx.buildArgs, fx.ignore)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	return plan
}

func (fx fixture) snap(t *testing.T) *snapshot.Snapshot {
	t.Helper()
	return snapshot.New(fx.plan(t), "Dockerfile", time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC))
}

const twoStage = `FROM golang:1.22 AS build
COPY go.mod ./
RUN go mod download
COPY src/ ./src/
RUN go build -o /bin/app ./src
FROM alpine:3.20
RUN adduser -D app
COPY --from=build /bin/app /usr/local/bin/app
CMD ["app"]
`

func twoStageFixture() fixture {
	return fixture{
		dockerfile: twoStage,
		files: map[string]fileSpec{
			"go.mod":        plain("module app"),
			"src/main.go":   plain("package main v1"),
			"src/server.go": plain("package main srv"),
		},
	}
}

func TestCompareNoChanges(t *testing.T) {
	fx := twoStageFixture()
	res := Compare(fx.snap(t), fx.plan(t))
	if res.Busted {
		t.Fatalf("busted = true: %+v", res.Culprit)
	}
	if res.CachedSteps != 9 || res.TotalSteps != 9 || res.RebuildSteps != 0 {
		t.Fatalf("counts = %+v", res)
	}
}

func TestCompareFileModifiedPinsExactFile(t *testing.T) {
	fx := twoStageFixture()
	old := fx.snap(t)
	fx.files["src/server.go"] = plain("package main srv EDITED")
	res := Compare(old, fx.plan(t))
	if !res.Busted || res.Culprit.Kind != KindFilesChanged {
		t.Fatalf("culprit = %+v", res.Culprit)
	}
	if res.Culprit.Step.Line != 4 || res.Culprit.Step.Index != 4 {
		t.Fatalf("culprit step = %+v (want the COPY src/ line)", res.Culprit.Step)
	}
	if len(res.Culprit.FileChanges) != 1 {
		t.Fatalf("file changes = %+v", res.Culprit.FileChanges)
	}
	fc := res.Culprit.FileChanges[0]
	if fc.Kind != "modified" || fc.Path != "src/server.go" {
		t.Fatalf("file change = %+v", fc)
	}
	if fc.OldDigest == fc.NewDigest {
		t.Fatal("digests must differ")
	}
}

func TestCompareBlastRadiusCascadesAcrossStages(t *testing.T) {
	fx := twoStageFixture()
	old := fx.snap(t)
	fx.files["src/server.go"] = plain("edited")
	res := Compare(old, fx.plan(t))
	// Stage build: steps 4-5 rebuild. Stage 1: step 7 (RUN adduser) stays
	// cached; steps 8-9 rebuild via COPY --from=build.
	if res.RebuildSteps != 4 || res.CachedSteps != 5 {
		t.Fatalf("rebuild=%d cached=%d", res.RebuildSteps, res.CachedSteps)
	}
	if len(res.Rebuilds) != 2 {
		t.Fatalf("rebuilds = %+v", res.Rebuilds)
	}
	b := res.Rebuilds[0]
	if b.Stage != "build" || b.FirstIndex != 4 || b.LastIndex != 5 || b.Steps != 2 || b.ViaStage != "" {
		t.Fatalf("build impact = %+v", b)
	}
	r := res.Rebuilds[1]
	if r.Stage != "stage-1" || r.FirstIndex != 8 || r.LastIndex != 9 || r.Steps != 2 {
		t.Fatalf("runtime impact = %+v", r)
	}
	if r.ViaCmd != "COPY" || r.ViaStage != "build" || r.ViaLine != 8 {
		t.Fatalf("via = %+v (must name the COPY --from edge)", r)
	}
}

func TestCompareIndependentStageStaysCached(t *testing.T) {
	// A change confined to the runtime stage must not drag the build stage
	// into the blast radius.
	fx := twoStageFixture()
	old := fx.snap(t)
	fx.dockerfile = strings.Replace(fx.dockerfile, "adduser -D app", "adduser -D app2", 1)
	res := Compare(old, fx.plan(t))
	if !res.Busted || res.Culprit.Step.Index != 7 {
		t.Fatalf("culprit = %+v", res.Culprit)
	}
	if res.Culprit.Kind != KindInstructionChanged {
		t.Fatalf("kind = %s", res.Culprit.Kind)
	}
	if len(res.Rebuilds) != 1 || res.Rebuilds[0].Stage != "stage-1" {
		t.Fatalf("rebuilds = %+v", res.Rebuilds)
	}
	if res.RebuildSteps != 3 { // steps 7, 8, 9
		t.Fatalf("rebuild steps = %d", res.RebuildSteps)
	}
}

func TestCompareInstructionChangedShowsOldAndNew(t *testing.T) {
	fx := twoStageFixture()
	old := fx.snap(t)
	fx.dockerfile = strings.Replace(fx.dockerfile, "go build -o /bin/app", "go build -tags prod -o /bin/app", 1)
	res := Compare(old, fx.plan(t))
	c := res.Culprit
	if c.Kind != KindInstructionChanged || c.Step.Line != 5 {
		t.Fatalf("culprit = %+v", c)
	}
	if !strings.Contains(c.OldText, "go build -o") || !strings.Contains(c.Step.Text, "-tags prod") {
		t.Fatalf("old=%q new=%q", c.OldText, c.Step.Text)
	}
}

func TestCompareFileAddedAndRemoved(t *testing.T) {
	fx := twoStageFixture()
	old := fx.snap(t)
	delete(fx.files, "src/server.go")
	fx.files["src/handler.go"] = plain("new file")
	res := Compare(old, fx.plan(t))
	if res.Culprit.Kind != KindFilesChanged || len(res.Culprit.FileChanges) != 2 {
		t.Fatalf("culprit = %+v", res.Culprit)
	}
	if res.Culprit.FileChanges[0].Kind != "added" || res.Culprit.FileChanges[0].Path != "src/handler.go" {
		t.Fatalf("first change = %+v", res.Culprit.FileChanges[0])
	}
	if res.Culprit.FileChanges[1].Kind != "removed" || res.Culprit.FileChanges[1].Path != "src/server.go" {
		t.Fatalf("second change = %+v", res.Culprit.FileChanges[1])
	}
	// A chmod with identical content is its own change kind: Docker's
	// COPY checksums include mode bits, and users rarely suspect them.
	fx = twoStageFixture()
	old = fx.snap(t)
	fx.files["src/server.go"] = fileSpec{content: "package main srv", mode: "0755"}
	res = Compare(old, fx.plan(t))
	fc := res.Culprit.FileChanges
	if len(fc) != 1 || fc[0].Kind != "mode-changed" || fc[0].OldMode != "0644" || fc[0].NewMode != "0755" {
		t.Fatalf("file changes = %+v", fc)
	}
}

func TestCompareArgChanged(t *testing.T) {
	fx := fixture{dockerfile: "FROM alpine\nARG CACHE_BUST=0\nRUN expensive-setup\n"}
	old := fx.snap(t)
	fx.buildArgs = map[string]string{"CACHE_BUST": "20260712"}
	res := Compare(old, fx.plan(t))
	c := res.Culprit
	if c.Kind != KindArgChanged || c.Step.Line != 3 {
		t.Fatalf("culprit = %+v (the RUN, not the ARG line, takes the miss)", c)
	}
	if len(c.ArgChanges) != 1 || c.ArgChanges[0].Name != "CACHE_BUST" || c.ArgChanges[0].New != "20260712" {
		t.Fatalf("arg changes = %+v", c.ArgChanges)
	}
}

func TestCompareTailGrowthAndShrink(t *testing.T) {
	// Appending a step: only the new step "rebuilds".
	fx := fixture{dockerfile: "FROM alpine\nRUN one\n"}
	old := fx.snap(t)
	fx.dockerfile = "FROM alpine\nRUN one\nRUN two\n"
	res := Compare(old, fx.plan(t))
	if res.Culprit.Kind != KindStepAdded || res.Culprit.Step.Index != 3 {
		t.Fatalf("culprit = %+v", res.Culprit)
	}
	if res.CachedSteps != 2 || res.RebuildSteps != 1 {
		t.Fatalf("counts = %+v", res)
	}
	// Removing a tail step: busted (the image changes) but zero rebuilds.
	fx = fixture{dockerfile: "FROM alpine\nRUN one\nRUN two\n"}
	old = fx.snap(t)
	fx.dockerfile = "FROM alpine\nRUN one\n"
	res = Compare(old, fx.plan(t))
	if !res.Busted || res.Culprit.Kind != KindStepRemoved {
		t.Fatalf("culprit = %+v", res.Culprit)
	}
	if res.RebuildSteps != 0 || res.CachedSteps != 2 {
		t.Fatalf("counts = %+v (removing a tail step rebuilds nothing)", res)
	}
}

func TestCompareAlsoBustedListsIndependentChanges(t *testing.T) {
	fx := twoStageFixture()
	old := fx.snap(t)
	// Change go.mod (busts build stage at step 2) AND the runtime RUN
	// (step 7) — the second change is invisible in the cascade of the
	// first only if buildbust reports it separately.
	fx.files["go.mod"] = plain("module app v2")
	fx.dockerfile = strings.Replace(fx.dockerfile, "adduser -D app", "adduser -D app2", 1)
	res := Compare(old, fx.plan(t))
	if res.Culprit.Step.Index != 2 {
		t.Fatalf("culprit = %+v", res.Culprit)
	}
	if len(res.AlsoBusted) != 1 || res.AlsoBusted[0].Index != 7 {
		t.Fatalf("also busted = %+v", res.AlsoBusted)
	}
}

func TestCompareIgnoreSuspectFlagged(t *testing.T) {
	fx := fixture{
		dockerfile: "FROM alpine\nCOPY . /app\n",
		files:      map[string]fileSpec{"a.txt": plain("a")},
		ignore:     cachekey.IgnoreInfo{Present: true, Digest: "sha256:v1", Patterns: []string{"*.log"}},
	}
	old := fx.snap(t)
	// The .dockerignore was edited and a file that used to be excluded now
	// shows up in the COPY set.
	fx.files["debug.log"] = plain("log")
	fx.ignore = cachekey.IgnoreInfo{Present: true, Digest: "sha256:v2", Patterns: nil}
	res := Compare(old, fx.plan(t))
	if !res.IgnoreChanged {
		t.Fatal("ignore change not detected")
	}
	if !res.Culprit.IgnoreSuspect {
		t.Fatalf("culprit = %+v (added file + changed ignore must raise the suspect flag)", res.Culprit)
	}
}

func TestCompareFromLineChange(t *testing.T) {
	fx := fixture{dockerfile: "FROM alpine:3.19\nRUN build\n"}
	old := fx.snap(t)
	fx.dockerfile = "FROM alpine:3.20\nRUN build\n"
	res := Compare(old, fx.plan(t))
	if res.Culprit.Step.Index != 1 || res.Culprit.Kind != KindInstructionChanged {
		t.Fatalf("culprit = %+v", res.Culprit)
	}
	if res.CachedSteps != 0 {
		t.Fatal("a FROM change rebuilds the whole stage")
	}
}
