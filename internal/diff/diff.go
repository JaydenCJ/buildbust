// Package diff compares a stored snapshot against a freshly keyed plan and
// produces the culprit report: the first step whose cache key diverged,
// why it diverged (down to the exact file), and the blast radius — which
// steps in which stages rebuild as a consequence.
package diff

import (
	"maps"
	"sort"

	"github.com/JaydenCJ/buildbust/internal/cachekey"
	"github.com/JaydenCJ/buildbust/internal/contextscan"
	"github.com/JaydenCJ/buildbust/internal/snapshot"
)

// Kind classifies why the culprit step's cache key diverged.
type Kind string

const (
	// KindInstructionChanged: the resolved instruction text differs
	// (edited line, inserted/removed instruction, expanded variable).
	KindInstructionChanged Kind = "instruction-changed"
	// KindFilesChanged: same instruction, but the context files it pulls
	// in differ in content, mode, or membership.
	KindFilesChanged Kind = "files-changed"
	// KindArgChanged: same instruction and files, but a --build-arg (or
	// ARG default) consumed by this RUN step changed value.
	KindArgChanged Kind = "arg-changed"
	// KindStepAdded / KindStepRemoved: the Dockerfile grew or shrank at
	// the tail with every earlier step still matching.
	KindStepAdded   Kind = "step-added"
	KindStepRemoved Kind = "step-removed"
	// KindKeyChanged: keys differ but no observable field does — usually
	// a snapshot from a different buildbust key schema.
	KindKeyChanged Kind = "key-changed"
)

// FileChange is one context-file difference under a COPY/ADD source.
type FileChange struct {
	// Kind is "modified", "added", "removed", or "mode-changed".
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	OldDigest string `json:"old_digest,omitempty"`
	NewDigest string `json:"new_digest,omitempty"`
	OldMode   string `json:"old_mode,omitempty"`
	NewMode   string `json:"new_mode,omitempty"`
}

// ArgChange is one build-arg value difference seen by a RUN step.
type ArgChange struct {
	Name string `json:"name"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

// Culprit is the first step whose key diverged, with the evidence.
type Culprit struct {
	Step        cachekey.Step `json:"step"`
	Kind        Kind          `json:"kind"`
	OldText     string        `json:"old_text,omitempty"`
	FileChanges []FileChange  `json:"file_changes,omitempty"`
	ArgChanges  []ArgChange   `json:"arg_changes,omitempty"`
	// IgnoreSuspect is set when files entered or left the instruction's
	// file set while .dockerignore also changed — the usual smoking gun
	// for pattern edits.
	IgnoreSuspect bool `json:"ignore_suspect"`
}

// StageImpact summarizes the rebuild inside one stage.
type StageImpact struct {
	Stage      string `json:"stage"`
	FirstIndex int    `json:"first_index"`
	LastIndex  int    `json:"last_index"`
	Steps      int    `json:"steps"`
	// ViaCmd/ViaStage/ViaLine describe the dependency edge that dragged
	// this stage into the rebuild ("" for the culprit's own stage).
	ViaCmd   string `json:"via_cmd,omitempty"`
	ViaStage string `json:"via_stage,omitempty"`
	ViaLine  int    `json:"via_line,omitempty"`
}

// Result is the full comparison outcome.
type Result struct {
	Busted        bool          `json:"busted"`
	TotalSteps    int           `json:"total_steps"`
	CachedSteps   int           `json:"cached_steps"`
	RebuildSteps  int           `json:"rebuild_steps"`
	IgnoreChanged bool          `json:"ignore_changed"`
	Culprit       *Culprit      `json:"culprit,omitempty"`
	Rebuilds      []StageImpact `json:"rebuilds,omitempty"`
	// AlsoBusted lists steps outside the culprit's cascade whose keys
	// independently diverged (only computed when step counts align).
	AlsoBusted []cachekey.Step `json:"also_busted,omitempty"`
}

// Compare walks both step lists in build order and reports the first key
// divergence plus its cascade.
func Compare(old *snapshot.Snapshot, cur *cachekey.Plan) *Result {
	res := &Result{
		TotalSteps:    len(cur.Steps),
		IgnoreChanged: old.Ignore.Digest != cur.Ignore.Digest || old.Ignore.Present != cur.Ignore.Present,
	}
	n := min(len(old.Steps), len(cur.Steps))
	culpritIdx := -1
	for i := 0; i < n; i++ {
		if old.Steps[i].Key != cur.Steps[i].Key {
			culpritIdx = i
			break
		}
	}
	if culpritIdx == -1 {
		if len(old.Steps) == len(cur.Steps) {
			res.CachedSteps = res.TotalSteps
			return res
		}
		culpritIdx = n // tail grew or shrank
	}
	res.Busted = true
	res.Culprit = classify(old, cur, culpritIdx, res.IgnoreChanged)

	invalid := make([]bool, len(cur.Steps))
	bustedStages := map[string]bool{}
	if culpritIdx < len(cur.Steps) {
		markStageFrom(cur, culpritIdx, invalid, bustedStages)
	}
	// Propagate across stage dependencies until stable. Dependencies only
	// point backwards, but a single stage can drag several others in.
	for changed := true; changed; {
		changed = false
		for i, s := range cur.Steps {
			if invalid[i] || s.DependsOnStage == "" {
				continue
			}
			if bustedStages[s.DependsOnStage] {
				markStageFrom(cur, i, invalid, bustedStages)
				changed = true
			}
		}
	}

	res.Rebuilds = stageImpacts(cur, invalid, res.Culprit)
	if len(old.Steps) == len(cur.Steps) {
		for i := culpritIdx + 1; i < len(cur.Steps); i++ {
			if !invalid[i] && old.Steps[i].Key != cur.Steps[i].Key {
				res.AlsoBusted = append(res.AlsoBusted, cur.Steps[i])
			}
		}
	}
	for _, inv := range invalid {
		if inv {
			res.RebuildSteps++
		}
	}
	res.CachedSteps = res.TotalSteps - res.RebuildSteps
	return res
}

// classify works out why the step at index i missed.
func classify(old *snapshot.Snapshot, cur *cachekey.Plan, i int, ignoreChanged bool) *Culprit {
	if i >= len(cur.Steps) {
		// The Dockerfile lost its tail: every remaining step still hits,
		// so nothing rebuilds, but the image will differ.
		o := old.Steps[i]
		return &Culprit{Step: o, Kind: KindStepRemoved, OldText: o.Text}
	}
	c := cur.Steps[i]
	if i >= len(old.Steps) {
		return &Culprit{Step: c, Kind: KindStepAdded}
	}
	o := old.Steps[i]
	culprit := &Culprit{Step: c}
	switch {
	case o.Text != c.Text || o.Cmd != c.Cmd:
		culprit.Kind = KindInstructionChanged
		culprit.OldText = o.Text
	case o.FilesDigest != c.FilesDigest:
		culprit.Kind = KindFilesChanged
		culprit.FileChanges = diffFiles(o.Files, c.Files)
		if ignoreChanged {
			for _, fc := range culprit.FileChanges {
				if fc.Kind == "added" || fc.Kind == "removed" {
					culprit.IgnoreSuspect = true
					break
				}
			}
		}
	case !maps.Equal(o.ArgsUsed, c.ArgsUsed):
		culprit.Kind = KindArgChanged
		culprit.ArgChanges = diffArgs(o.ArgsUsed, c.ArgsUsed)
	default:
		culprit.Kind = KindKeyChanged
		culprit.OldText = o.Text
	}
	return culprit
}

func diffFiles(old, cur []contextscan.FileEntry) []FileChange {
	oldBy := make(map[string]contextscan.FileEntry, len(old))
	for _, f := range old {
		oldBy[f.Path] = f
	}
	curBy := make(map[string]contextscan.FileEntry, len(cur))
	for _, f := range cur {
		curBy[f.Path] = f
	}
	paths := make([]string, 0, len(oldBy)+len(curBy))
	for p := range oldBy {
		paths = append(paths, p)
	}
	for p := range curBy {
		if _, ok := oldBy[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	var out []FileChange
	for _, p := range paths {
		o, hadOld := oldBy[p]
		c, hasCur := curBy[p]
		switch {
		case hadOld && !hasCur:
			out = append(out, FileChange{Kind: "removed", Path: p, OldDigest: o.Digest, OldMode: o.Mode})
		case !hadOld && hasCur:
			out = append(out, FileChange{Kind: "added", Path: p, NewDigest: c.Digest, NewMode: c.Mode})
		case o.Digest != c.Digest:
			out = append(out, FileChange{Kind: "modified", Path: p, OldDigest: o.Digest, NewDigest: c.Digest, OldMode: o.Mode, NewMode: c.Mode})
		case o.Mode != c.Mode:
			out = append(out, FileChange{Kind: "mode-changed", Path: p, OldDigest: o.Digest, NewDigest: c.Digest, OldMode: o.Mode, NewMode: c.Mode})
		}
	}
	return out
}

func diffArgs(old, cur map[string]string) []ArgChange {
	names := map[string]bool{}
	for k := range old {
		names[k] = true
	}
	for k := range cur {
		names[k] = true
	}
	sorted := make([]string, 0, len(names))
	for k := range names {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var out []ArgChange
	for _, k := range sorted {
		if old[k] != cur[k] {
			out = append(out, ArgChange{Name: k, Old: old[k], New: cur[k]})
		}
	}
	return out
}

// markStageFrom invalidates every step of the given step's stage from that
// step onward (stages are contiguous in build order).
func markStageFrom(cur *cachekey.Plan, from int, invalid []bool, busted map[string]bool) {
	stage := cur.Steps[from].Stage
	for j := from; j < len(cur.Steps); j++ {
		if cur.Steps[j].Stage == stage {
			invalid[j] = true
		}
	}
	busted[stage] = true
}

// stageImpacts groups invalidated steps per stage, in build order, with
// the dependency edge that pulled non-culprit stages in.
func stageImpacts(cur *cachekey.Plan, invalid []bool, culprit *Culprit) []StageImpact {
	var out []StageImpact
	seen := map[string]int{} // stage → index into out
	for i, s := range cur.Steps {
		if !invalid[i] {
			continue
		}
		idx, ok := seen[s.Stage]
		if !ok {
			impact := StageImpact{Stage: s.Stage, FirstIndex: s.Index, LastIndex: s.Index, Steps: 1}
			if culprit != nil && s.Stage != culprit.Step.Stage {
				impact.ViaCmd = s.Cmd
				impact.ViaStage = s.DependsOnStage
				impact.ViaLine = s.Line
			}
			out = append(out, impact)
			seen[s.Stage] = len(out) - 1
			continue
		}
		out[idx].LastIndex = s.Index
		out[idx].Steps++
	}
	return out
}
