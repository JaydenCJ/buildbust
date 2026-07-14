// Package render formats plans and comparison results for humans (text)
// and machines (stable JSON, schema_version 1). All orderings are
// deterministic: identical input produces byte-identical output.
package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/JaydenCJ/buildbust/internal/cachekey"
	"github.com/JaydenCJ/buildbust/internal/diff"
)

// Meta carries the run context shown in report headers.
type Meta struct {
	Dockerfile   string `json:"dockerfile"`
	Context      string `json:"context"`
	SnapshotPath string `json:"snapshot,omitempty"`
	SnapshotAt   string `json:"snapshot_created_at,omitempty"`
}

// ExplainText renders the culprit report.
func ExplainText(w io.Writer, res *diff.Result, meta Meta) {
	fmt.Fprintf(w, "buildbust explain — Dockerfile %s, context %s\n\n", meta.Dockerfile, meta.Context)
	if !res.Busted {
		fmt.Fprintf(w, "CACHE OK — all %d steps still cached against snapshot from %s\n", res.TotalSteps, meta.SnapshotAt)
		if res.IgnoreChanged {
			fmt.Fprintf(w, "note: .dockerignore changed since the snapshot, but no COPY/ADD file set moved\n")
		}
		return
	}
	c := res.Culprit
	switch c.Kind {
	case diff.KindStepRemoved:
		fmt.Fprintf(w, "DOCKERFILE SHRANK — step %d was removed from the tail\n\n", c.Step.Index)
		fmt.Fprintf(w, "  removed: %s\n", firstLine(c.OldText))
		fmt.Fprintf(w, "\n  no cached work is lost: all %d remaining steps still hit\n", res.TotalSteps)
		return
	case diff.KindStepAdded:
		fmt.Fprintf(w, "CACHE BUSTED at step %d/%d — stage %q, line %d (new step appended)\n\n",
			c.Step.Index, res.TotalSteps, c.Step.Stage, c.Step.Line)
	default:
		fmt.Fprintf(w, "CACHE BUSTED at step %d/%d — stage %q, line %d\n\n",
			c.Step.Index, res.TotalSteps, c.Step.Stage, c.Step.Line)
	}
	fmt.Fprintf(w, "    %d | %s\n\n", c.Step.Line, firstLine(c.Step.Text))

	switch c.Kind {
	case diff.KindInstructionChanged:
		fmt.Fprintf(w, "  cause: the instruction itself changed\n")
		fmt.Fprintf(w, "    was: %s\n", firstLine(c.OldText))
		fmt.Fprintf(w, "    now: %s\n", firstLine(c.Step.Text))
	case diff.KindFilesChanged:
		fmt.Fprintf(w, "  cause: build context changed under %s sources [%s]\n",
			c.Step.Cmd, strings.Join(c.Step.Sources, ", "))
		for _, fc := range c.FileChanges {
			switch fc.Kind {
			case "modified":
				fmt.Fprintf(w, "    ~ modified      %s   %s → %s\n", fc.Path, shortDigest(fc.OldDigest), shortDigest(fc.NewDigest))
			case "added":
				fmt.Fprintf(w, "    + added         %s   %s\n", fc.Path, shortDigest(fc.NewDigest))
			case "removed":
				fmt.Fprintf(w, "    - removed       %s   %s\n", fc.Path, shortDigest(fc.OldDigest))
			case "mode-changed":
				fmt.Fprintf(w, "    ~ mode changed  %s   %s → %s\n", fc.Path, fc.OldMode, fc.NewMode)
			}
		}
		if c.IgnoreSuspect {
			fmt.Fprintf(w, "    (.dockerignore also changed since the snapshot — pattern edits pull files in and out)\n")
		}
	case diff.KindArgChanged:
		fmt.Fprintf(w, "  cause: a build arg consumed by this RUN changed value\n")
		for _, ac := range c.ArgChanges {
			fmt.Fprintf(w, "    ~ %s: %q → %q\n", ac.Name, ac.Old, ac.New)
		}
	case diff.KindStepAdded:
		fmt.Fprintf(w, "  cause: this step did not exist in the snapshot\n")
	case diff.KindKeyChanged:
		fmt.Fprintf(w, "  cause: cache key changed with no visible field difference — re-snapshot with this buildbust version\n")
	}

	fmt.Fprintf(w, "\n  blast radius: %d of %d steps rebuild, %d stay cached\n",
		res.RebuildSteps, res.TotalSteps, res.CachedSteps)
	for _, imp := range res.Rebuilds {
		span := fmt.Sprintf("steps %d-%d", imp.FirstIndex, imp.LastIndex)
		if imp.FirstIndex == imp.LastIndex {
			span = fmt.Sprintf("step %d", imp.FirstIndex)
		}
		line := fmt.Sprintf("    stage %-12s %-12s %d step", imp.Stage, span, imp.Steps)
		if imp.Steps != 1 {
			line += "s"
		}
		if imp.ViaStage != "" {
			line += fmt.Sprintf("   via %s --from=%s (line %d)", imp.ViaCmd, imp.ViaStage, imp.ViaLine)
		}
		fmt.Fprintln(w, line)
	}
	if len(res.AlsoBusted) > 0 {
		fmt.Fprintf(w, "\n  also changed independently (outside this cascade):\n")
		for _, s := range res.AlsoBusted {
			fmt.Fprintf(w, "    step %d (stage %q, line %d): %s\n", s.Index, s.Stage, s.Line, firstLine(s.Text))
		}
	}
	if res.IgnoreChanged && (c.Kind != diff.KindFilesChanged || !c.IgnoreSuspect) {
		fmt.Fprintf(w, "\n  note: .dockerignore changed since the snapshot\n")
	}
}

// FilesText renders which context files feed each COPY/ADD cache key.
func FilesText(w io.Writer, plan *cachekey.Plan, meta Meta) {
	fmt.Fprintf(w, "buildbust files — Dockerfile %s, context %s\n", meta.Dockerfile, meta.Context)
	shown := 0
	for _, s := range plan.Steps {
		if s.Cmd != "COPY" && s.Cmd != "ADD" {
			continue
		}
		shown++
		fmt.Fprintf(w, "\nstep %d  line %d  %s\n", s.Index, s.Line, firstLine(s.Text))
		if s.DependsOnStage != "" {
			fmt.Fprintf(w, "  copies from stage %q (no context files)\n", s.DependsOnStage)
		} else if len(s.Files) > 0 || s.FilesDigest != "" {
			var total int64
			for _, f := range s.Files {
				total += f.Size
			}
			fmt.Fprintf(w, "  %d file%s, %d B, digest %s\n", len(s.Files), plural(len(s.Files)), total, shortDigest(s.FilesDigest))
			for _, f := range s.Files {
				fmt.Fprintf(w, "    %s  %s\n", f.Mode, f.Path)
			}
		}
		for _, n := range s.Notes {
			fmt.Fprintf(w, "  note: %s\n", n)
		}
	}
	if shown == 0 {
		fmt.Fprintf(w, "\nno COPY or ADD instructions read from the build context\n")
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

func shortDigest(d string) string {
	hex := strings.TrimPrefix(d, "sha256:")
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return hex
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
