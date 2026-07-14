// Tests for the renderers: the text report must carry every load-bearing
// fact (step, line, file, digest), and the JSON envelope must be stable
// and machine-parseable.
package render

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/JaydenCJ/buildbust/internal/cachekey"
	"github.com/JaydenCJ/buildbust/internal/contextscan"
	"github.com/JaydenCJ/buildbust/internal/diff"
)

func sampleMeta() Meta {
	return Meta{Dockerfile: "Dockerfile", Context: ".", SnapshotPath: ".buildbust.json", SnapshotAt: "2026-07-12T09:00:00Z"}
}

func TestExplainTextCacheOK(t *testing.T) {
	var buf bytes.Buffer
	ExplainText(&buf, &diff.Result{TotalSteps: 9, CachedSteps: 9}, sampleMeta())
	out := buf.String()
	if !strings.Contains(out, "CACHE OK") || !strings.Contains(out, "all 9 steps") {
		t.Fatalf("output:\n%s", out)
	}
	if !strings.Contains(out, "2026-07-12T09:00:00Z") {
		t.Fatalf("snapshot timestamp missing:\n%s", out)
	}
}

func TestExplainTextCulpritCarriesEvidence(t *testing.T) {
	res := &diff.Result{
		Busted: true, TotalSteps: 9, CachedSteps: 5, RebuildSteps: 4,
		Culprit: &diff.Culprit{
			Step: cachekey.Step{Index: 4, Stage: "build", Line: 4, Cmd: "COPY",
				Text: "COPY src/ ./src/", Sources: []string{"src"}},
			Kind: diff.KindFilesChanged,
			FileChanges: []diff.FileChange{
				{Kind: "modified", Path: "src/server.go", OldDigest: "sha256:aaaabbbbccccdddd", NewDigest: "sha256:eeeeffff00001111"},
				{Kind: "added", Path: "src/new.go", NewDigest: "sha256:2222333344445555"},
			},
		},
		Rebuilds: []diff.StageImpact{
			{Stage: "build", FirstIndex: 4, LastIndex: 5, Steps: 2},
			{Stage: "stage-1", FirstIndex: 8, LastIndex: 9, Steps: 2, ViaCmd: "COPY", ViaStage: "build", ViaLine: 8},
		},
	}
	var buf bytes.Buffer
	ExplainText(&buf, res, sampleMeta())
	out := buf.String()
	for _, want := range []string{
		"CACHE BUSTED at step 4/9",
		"line 4",
		"COPY src/ ./src/",
		"src/server.go",
		"aaaabbbbcccc → eeeeffff0000", // digests shortened to 12 hex chars
		"+ added",
		"src/new.go",
		"4 of 9 steps rebuild, 5 stay cached",
		"via COPY --from=build (line 8)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestExplainTextArgCulprit(t *testing.T) {
	res := &diff.Result{
		Busted: true, TotalSteps: 3, CachedSteps: 2, RebuildSteps: 1,
		Culprit: &diff.Culprit{
			Step:       cachekey.Step{Index: 3, Stage: "stage-0", Line: 3, Cmd: "RUN", Text: "RUN expensive"},
			Kind:       diff.KindArgChanged,
			ArgChanges: []diff.ArgChange{{Name: "VER", Old: "1", New: "2"}},
		},
		Rebuilds: []diff.StageImpact{{Stage: "stage-0", FirstIndex: 3, LastIndex: 3, Steps: 1}},
	}
	var buf bytes.Buffer
	ExplainText(&buf, res, sampleMeta())
	out := buf.String()
	if !strings.Contains(out, `~ VER: "1" → "2"`) {
		t.Fatalf("arg evidence missing:\n%s", out)
	}
	if !strings.Contains(out, "step 3") {
		t.Fatalf("step span missing:\n%s", out)
	}
}

func TestExplainTextStepRemoved(t *testing.T) {
	res := &diff.Result{
		Busted: true, TotalSteps: 2, CachedSteps: 2,
		Culprit: &diff.Culprit{
			Step:    cachekey.Step{Index: 3, Stage: "stage-0", Line: 3, Cmd: "RUN", Text: "RUN two"},
			Kind:    diff.KindStepRemoved,
			OldText: "RUN two",
		},
	}
	var buf bytes.Buffer
	ExplainText(&buf, res, sampleMeta())
	out := buf.String()
	if !strings.Contains(out, "DOCKERFILE SHRANK") || !strings.Contains(out, "no cached work is lost") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestExplainJSONShape(t *testing.T) {
	res := &diff.Result{
		Busted: true, TotalSteps: 2, CachedSteps: 1, RebuildSteps: 1,
		Culprit: &diff.Culprit{
			Step: cachekey.Step{Index: 2, Stage: "stage-0", Line: 2, Cmd: "COPY", Text: "COPY . /app"},
			Kind: diff.KindFilesChanged,
			FileChanges: []diff.FileChange{
				{Kind: "modified", Path: "a.txt", OldDigest: "sha256:old", NewDigest: "sha256:new"},
			},
		},
	}
	var buf bytes.Buffer
	if err := ExplainJSON(&buf, res, sampleMeta()); err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Tool          string `json:"tool"`
		SchemaVersion int    `json:"schema_version"`
		Busted        bool   `json:"busted"`
		Culprit       struct {
			Kind        string `json:"kind"`
			FileChanges []struct {
				Path string `json:"path"`
			} `json:"file_changes"`
		} `json:"culprit"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if parsed.Tool != "buildbust" || parsed.SchemaVersion != 1 || !parsed.Busted {
		t.Fatalf("envelope = %+v", parsed)
	}
	if parsed.Culprit.Kind != "files-changed" || parsed.Culprit.FileChanges[0].Path != "a.txt" {
		t.Fatalf("culprit = %+v", parsed.Culprit)
	}
}

func TestFilesTextListsPerStepInventory(t *testing.T) {
	plan := &cachekey.Plan{
		Stages: []string{"stage-0"},
		Steps: []cachekey.Step{
			{Index: 1, Stage: "stage-0", Line: 1, Cmd: "FROM", Text: "FROM alpine"},
			{Index: 2, Stage: "stage-0", Line: 2, Cmd: "COPY", Text: "COPY src/ /app", Sources: []string{"src"},
				Files: []contextscan.FileEntry{
					{Path: "src/a.go", Size: 10, Mode: "0644", Digest: "sha256:abcdefabcdefabcd"},
					{Path: "src/b.go", Size: 20, Mode: "0755", Digest: "sha256:1234561234561234"},
				},
				FilesDigest: "sha256:fffff00000fffff00000"},
			{Index: 3, Stage: "stage-0", Line: 3, Cmd: "COPY", Text: "COPY --from=build /x /x", DependsOnStage: "build"},
		},
	}
	var buf bytes.Buffer
	FilesText(&buf, plan, sampleMeta())
	out := buf.String()
	for _, want := range []string{
		"step 2  line 2  COPY src/ /app",
		"2 files, 30 B",
		"0644  src/a.go",
		"0755  src/b.go",
		`copies from stage "build" (no context files)`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "FROM alpine\n  ") {
		t.Fatal("non-COPY steps must not get inventory sections")
	}
	// A Dockerfile with no context COPY/ADD at all says so explicitly.
	empty := &cachekey.Plan{Stages: []string{"stage-0"}, Steps: []cachekey.Step{{Index: 1, Cmd: "FROM", Text: "FROM alpine"}}}
	buf.Reset()
	FilesText(&buf, empty, sampleMeta())
	if !strings.Contains(buf.String(), "no COPY or ADD instructions") {
		t.Fatalf("output:\n%s", buf.String())
	}
}

func TestFilesJSONFiltersToCopySteps(t *testing.T) {
	plan := &cachekey.Plan{
		Stages: []string{"stage-0"},
		Steps: []cachekey.Step{
			{Index: 1, Cmd: "FROM", Text: "FROM alpine"},
			{Index: 2, Cmd: "COPY", Text: "COPY a /a"},
			{Index: 3, Cmd: "RUN", Text: "RUN build"},
			{Index: 4, Cmd: "ADD", Text: "ADD b /b"},
		},
	}
	var buf bytes.Buffer
	if err := FilesJSON(&buf, plan, sampleMeta()); err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Steps []struct {
			Cmd string `json:"cmd"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Steps) != 2 || parsed.Steps[0].Cmd != "COPY" || parsed.Steps[1].Cmd != "ADD" {
		t.Fatalf("steps = %+v", parsed.Steps)
	}
}
