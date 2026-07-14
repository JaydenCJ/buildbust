// Tests for the build-context scanner: deterministic inventory, ignore
// pruning, symlink handling, and the combined digest. All fixtures live
// in t.TempDir() — no fixtures on disk, no network, no clock.
package contextscan

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/JaydenCJ/buildbust/internal/ignore"
)

// writeTree materializes path→content pairs under root.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func mustScan(t *testing.T, root string, m *ignore.Matcher) []FileEntry {
	t.Helper()
	entries, err := Scan(root, m, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return entries
}

func paths(entries []FileEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	return out
}

func TestScanSortsAndHashesDeterministically(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"b.txt":       "bee",
		"a.txt":       "ay",
		"sub/c.txt":   "sea",
		"sub/d/e.txt": "ee",
	})
	first := mustScan(t, root, nil)
	second := mustScan(t, root, nil)
	if len(first) != 4 {
		t.Fatalf("entries = %v", paths(first))
	}
	want := []string{"a.txt", "b.txt", "sub/c.txt", "sub/d/e.txt"}
	for i, p := range paths(first) {
		if p != want[i] {
			t.Fatalf("order = %v, want %v", paths(first), want)
		}
	}
	if Digest(first) != Digest(second) {
		t.Fatal("two scans of the same tree must produce identical digests")
	}
	// A well-known content hash pins the digest algorithm itself.
	if first[0].Digest != "sha256:69d61997a241e97931db9dd1cfcef218041a752485f5f7956b09766287682da3" {
		t.Fatalf("digest of %q = %s", "ay", first[0].Digest)
	}
}

func TestScanAppliesIgnoreRules(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"keep.go":   "package a",
		"drop.log":  "noise",
		"sub/x.log": "noise",
		"sub/y.go":  "package b",
	})
	m, err := ignore.FromPatterns([]string{"*.log", "sub/x.log"})
	if err != nil {
		t.Fatal(err)
	}
	got := paths(mustScan(t, root, m))
	if len(got) != 2 || got[0] != "keep.go" || got[1] != "sub/y.go" {
		t.Fatalf("kept = %v", got)
	}
}

func TestScanPrunesIgnoredDirectory(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"node_modules/pkg/deep/index.js": "js",
		"src/main.go":                    "go",
	})
	m, err := ignore.FromPatterns([]string{"node_modules"})
	if err != nil {
		t.Fatal(err)
	}
	got := paths(mustScan(t, root, m))
	if len(got) != 1 || got[0] != "src/main.go" {
		t.Fatalf("kept = %v", got)
	}
}

func TestScanNegationDescendsIntoIgnoredDirectory(t *testing.T) {
	// With a `!` pattern present, an ignored directory cannot be pruned:
	// something below it may be re-included.
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"dist/bundle.js": "js",
		"dist/keep.map":  "map",
	})
	m, err := ignore.FromPatterns([]string{"dist", "!dist/keep.map"})
	if err != nil {
		t.Fatal(err)
	}
	got := paths(mustScan(t, root, m))
	if len(got) != 1 || got[0] != "dist/keep.map" {
		t.Fatalf("kept = %v", got)
	}
}

func TestScanRecordsSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"real.txt": "content"})
	if err := os.Symlink("real.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	entries := mustScan(t, root, nil)
	var link *FileEntry
	for i := range entries {
		if entries[i].Path == "link.txt" {
			link = &entries[i]
		}
	}
	if link == nil {
		t.Fatalf("symlink missing from %v", paths(entries))
	}
	if link.Mode != "symlink" {
		t.Fatalf("mode = %q", link.Mode)
	}
	// The digest hashes the target string, not the target content — the
	// tarball stores the link itself, so retargeting must bust the cache
	// while touching the target's content must not (via this entry).
	other := mustScan(t, root, nil)
	if Digest(entries) != Digest(other) {
		t.Fatal("symlink digest must be stable")
	}
}

func TestScanExcludeExactPaths(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".buildbust.json": "{}",
		"app.go":          "package main",
	})
	entries, err := Scan(root, nil, map[string]bool{".buildbust.json": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "app.go" {
		t.Fatalf("kept = %v", paths(entries))
	}
}

func TestScanRecordsPermissionMode(t *testing.T) {
	// Docker's COPY checksums include mode bits: a `chmod +x` busts the
	// cache. The scanner must therefore record permissions faithfully.
	root := t.TempDir()
	writeTree(t, root, map[string]string{"run.sh": "#!/bin/sh\n"})
	if err := os.Chmod(filepath.Join(root, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	entries := mustScan(t, root, nil)
	if entries[0].Mode != "0755" {
		t.Fatalf("mode = %q, want 0755", entries[0].Mode)
	}
}

func TestDigestChangesWithContentAndMembership(t *testing.T) {
	a := []FileEntry{{Path: "x", Size: 1, Mode: "0644", Digest: "sha256:aa"}}
	b := []FileEntry{{Path: "x", Size: 1, Mode: "0644", Digest: "sha256:bb"}}
	c := []FileEntry{}
	if Digest(a) == Digest(b) {
		t.Fatal("content change must change the digest")
	}
	if Digest(a) == Digest(c) {
		t.Fatal("membership change must change the digest")
	}
	if Digest(c) != Digest([]FileEntry{}) {
		t.Fatal("empty digest must be stable")
	}
}

func TestScanMissingRootIsError(t *testing.T) {
	if _, err := Scan(filepath.Join(t.TempDir(), "absent"), nil, nil); err == nil {
		t.Fatal("want error for missing context directory")
	}
}
