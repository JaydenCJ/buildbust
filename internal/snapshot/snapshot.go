// Package snapshot persists a keyed build plan to disk as the baseline
// that later `buildbust explain` runs compare against. The format is plain
// indented JSON with a schema version, so it diffs cleanly in git and
// other tools can consume it.
package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/JaydenCJ/buildbust/internal/cachekey"
	"github.com/JaydenCJ/buildbust/internal/version"
)

// SchemaVersion is bumped whenever the snapshot format changes shape.
const SchemaVersion = 1

// Snapshot is the on-disk baseline.
type Snapshot struct {
	Tool          string              `json:"tool"`
	SchemaVersion int                 `json:"schema_version"`
	Version       string              `json:"version"`
	CreatedAt     string              `json:"created_at"`
	Dockerfile    string              `json:"dockerfile"`
	BuildArgs     map[string]string   `json:"build_args,omitempty"`
	Ignore        cachekey.IgnoreInfo `json:"ignore"`
	Stages        []string            `json:"stages"`
	Steps         []cachekey.Step     `json:"steps"`
}

// New wraps a plan into a snapshot. The clock is a parameter so tests stay
// deterministic.
func New(plan *cachekey.Plan, dockerfilePath string, now time.Time) *Snapshot {
	return &Snapshot{
		Tool:          "buildbust",
		SchemaVersion: SchemaVersion,
		Version:       version.Version,
		CreatedAt:     now.UTC().Format(time.RFC3339),
		Dockerfile:    dockerfilePath,
		BuildArgs:     plan.BuildArgs,
		Ignore:        plan.Ignore,
		Stages:        plan.Stages,
		Steps:         plan.Steps,
	}
}

// Write stores the snapshot as indented JSON with a trailing newline.
func Write(path string, s *Snapshot) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// Read loads and validates a snapshot file.
func Read(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", path, err)
	}
	if s.Tool != "buildbust" {
		return nil, fmt.Errorf("snapshot %s: not a buildbust snapshot (tool=%q)", path, s.Tool)
	}
	if s.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("snapshot %s: schema version %d not supported (want %d) — re-run `buildbust snapshot`", path, s.SchemaVersion, SchemaVersion)
	}
	return &s, nil
}
