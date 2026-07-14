// Package contextscan walks a Docker build context on disk, applies
// .dockerignore rules, and produces a deterministic content-hashed file
// inventory — the same set of files a `docker build` would send to the
// daemon, hashed the way the builder hashes them (content + mode, never
// mtime).
package contextscan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/JaydenCJ/buildbust/internal/ignore"
)

// FileEntry is one file (or symlink) in the build context.
type FileEntry struct {
	// Path is slash-separated and relative to the context root.
	Path string `json:"path"`
	Size int64  `json:"size"`
	// Mode is the octal permission string ("0644", "0755", …) or the
	// literal "symlink". Docker's cache checksums include mode bits, so a
	// chmod busts COPY caches exactly like a content edit does.
	Mode string `json:"mode"`
	// Digest is "sha256:<hex>" of the file content, or of the link target
	// string for symlinks.
	Digest string `json:"digest"`
}

// Scan walks root and returns every context file in sorted path order.
// m may be nil (no .dockerignore). excludeExact holds context-relative
// paths that are always skipped regardless of ignore rules — buildbust
// uses it to keep its own snapshot file out of the inventory.
func Scan(root string, m *ignore.Matcher, excludeExact map[string]bool) ([]FileEntry, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("build context %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("build context %q is not a directory", root)
	}
	var out []FileEntry
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			// Prune ignored subtrees, but only when no `!` pattern could
			// re-include something deeper down.
			if m.Ignored(rel) && !m.HasNegations() {
				return fs.SkipDir
			}
			return nil
		}
		if excludeExact[rel] || m.Ignored(rel) {
			return nil
		}
		switch mode := d.Type(); {
		case mode&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256([]byte(target))
			out = append(out, FileEntry{
				Path:   rel,
				Size:   int64(len(target)),
				Mode:   "symlink",
				Digest: "sha256:" + hex.EncodeToString(sum[:]),
			})
		case mode.IsRegular():
			entry, err := hashRegular(p, rel)
			if err != nil {
				return err
			}
			out = append(out, entry)
		default:
			// Sockets, devices and other irregular files never make it
			// into a build context tarball; skip them silently.
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func hashRegular(p, rel string) (FileEntry, error) {
	f, err := os.Open(p)
	if err != nil {
		return FileEntry{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return FileEntry{}, err
	}
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return FileEntry{}, fmt.Errorf("hashing %s: %w", rel, err)
	}
	return FileEntry{
		Path:   rel,
		Size:   size,
		Mode:   fmt.Sprintf("%04o", info.Mode().Perm()),
		Digest: "sha256:" + hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// Digest folds a sorted entry list into one combined digest. Any change to
// a member path, mode, content, or size changes the result; an empty list
// has a stable digest of its own.
func Digest(entries []FileEntry) string {
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d\n", e.Path, e.Mode, e.Digest, e.Size)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
