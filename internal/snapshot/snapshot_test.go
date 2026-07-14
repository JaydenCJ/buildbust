// Tests for snapshot persistence: round-tripping, validation, and the
// metadata New stamps in.
package snapshot

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JaydenCJ/buildbust/internal/cachekey"
	"github.com/JaydenCJ/buildbust/internal/version"
)

func samplePlan() *cachekey.Plan {
	return &cachekey.Plan{
		BuildArgs: map[string]string{"VER": "2"},
		Ignore:    cachekey.IgnoreInfo{Present: true, Source: ".dockerignore", Digest: "sha256:abc", Patterns: []string{"*.log"}},
		Stages:    []string{"build"},
		Steps: []cachekey.Step{
			{Index: 1, Stage: "build", Line: 1, Cmd: "FROM", Text: "FROM alpine", Key: "sha256:k1"},
			{Index: 2, Stage: "build", Line: 2, Cmd: "RUN", Text: "RUN build", Key: "sha256:k2", ArgsUsed: map[string]string{"VER": "2"}},
		},
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.json")
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	if err := Write(path, New(samplePlan(), "Dockerfile", now)); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt != "2026-07-12T10:00:00Z" {
		t.Fatalf("created_at = %q", got.CreatedAt)
	}
	if got.Dockerfile != "Dockerfile" || got.Version != version.Version {
		t.Fatalf("meta = %+v", got)
	}
	if len(got.Steps) != 2 || got.Steps[1].ArgsUsed["VER"] != "2" {
		t.Fatalf("steps = %+v", got.Steps)
	}
	if !got.Ignore.Present || got.Ignore.Patterns[0] != "*.log" {
		t.Fatalf("ignore = %+v", got.Ignore)
	}
	// Snapshot files get committed to git, so they must end with a newline.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatal("snapshot files must end with a newline")
	}
}

func TestReadRejectsForeignAndBrokenFiles(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"other-tool.json": `{"tool":"other","schema_version":1}`,
		"future.json":     `{"tool":"buildbust","schema_version":99}`,
		"garbage.json":    `not json at all`,
	}
	for name, content := range cases {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(p); err == nil {
			t.Fatalf("Read(%s): want error", name)
		}
	}
}

func TestReadMissingFileReportsNotExist(t *testing.T) {
	_, err := Read(filepath.Join(t.TempDir(), "absent.json"))
	if !os.IsNotExist(err) {
		t.Fatalf("err = %v, want IsNotExist (the CLI hints `buildbust snapshot` on it)", err)
	}
}
