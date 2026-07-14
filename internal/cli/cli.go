// Package cli implements the buildbust command-line interface. Run takes
// argv and two writers and returns an exit code, so the whole surface is
// testable in-process without building a binary.
package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JaydenCJ/buildbust/internal/cachekey"
	"github.com/JaydenCJ/buildbust/internal/contextscan"
	"github.com/JaydenCJ/buildbust/internal/diff"
	"github.com/JaydenCJ/buildbust/internal/dockerfile"
	"github.com/JaydenCJ/buildbust/internal/ignore"
	"github.com/JaydenCJ/buildbust/internal/render"
	"github.com/JaydenCJ/buildbust/internal/snapshot"
	"github.com/JaydenCJ/buildbust/internal/version"
)

// Exit codes. Documented in the README; `explain` uses ExitBusted as its
// machine-readable verdict.
const (
	ExitOK      = 0
	ExitBusted  = 1
	ExitUsage   = 2
	ExitRuntime = 3
)

// DefaultSnapshotName is the snapshot filename used when -o/--against is
// not given, resolved inside the build context.
const DefaultSnapshotName = ".buildbust.json"

// Run dispatches argv and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stdout)
		return ExitOK
	}
	switch args[0] {
	case "snapshot":
		return runSnapshot(args[1:], stdout, stderr)
	case "explain":
		return runExplain(args[1:], stdout, stderr)
	case "files":
		return runFiles(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "buildbust %s\n", version.Version)
		return ExitOK
	case "help", "--help", "-h":
		usage(stdout)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "buildbust: unknown command %q\n\n", args[0])
		usage(stderr)
		return ExitUsage
	}
}

// multiFlag is a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// buildFlags are shared by snapshot, explain, and files.
type buildFlags struct {
	file         string
	dockerignore string
	buildArgs    multiFlag
}

func (b *buildFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&b.file, "f", "", "path to the Dockerfile (default: <context>/Dockerfile)")
	fs.StringVar(&b.file, "file", "", "path to the Dockerfile (default: <context>/Dockerfile)")
	fs.StringVar(&b.dockerignore, "dockerignore", "", "path to the ignore file (default: auto-detected in the context)")
	fs.Var(&b.buildArgs, "build-arg", "build-time variable NAME=value (repeatable)")
}

// loadResult bundles everything a subcommand needs about the build.
type loadResult struct {
	plan           *cachekey.Plan
	contextDir     string
	dockerfilePath string
	contextFiles   int
}

// load resolves paths, parses the Dockerfile, scans the context, and keys
// the plan. snapshotPath (when non-empty and inside the context) is
// excluded from the scan so the tool never reports itself as the culprit.
func load(bf *buildFlags, contextDir, snapshotPath string) (*loadResult, error) {
	dfPath := bf.file
	if dfPath == "" {
		dfPath = filepath.Join(contextDir, "Dockerfile")
	}
	dfData, err := os.ReadFile(dfPath)
	if err != nil {
		return nil, fmt.Errorf("dockerfile: %w", err)
	}
	df, err := dockerfile.Parse(strings.NewReader(string(dfData)))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dfPath, err)
	}

	buildArgs := map[string]string{}
	for _, spec := range bf.buildArgs {
		name, value, ok := strings.Cut(spec, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("invalid --build-arg %q: want NAME=value (buildbust never reads the process environment, for determinism)", spec)
		}
		buildArgs[name] = value
	}

	matcher, info, err := resolveIgnore(bf, contextDir, dfPath)
	if err != nil {
		return nil, err
	}

	excludeExact := map[string]bool{}
	if rel, ok := insideContext(contextDir, snapshotPath); ok {
		excludeExact[rel] = true
	}
	files, err := contextscan.Scan(contextDir, matcher, excludeExact)
	if err != nil {
		return nil, err
	}
	plan, err := cachekey.Compute(df, files, buildArgs, info)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dfPath, err)
	}
	return &loadResult{plan: plan, contextDir: contextDir, dockerfilePath: dfPath, contextFiles: len(files)}, nil
}

// resolveIgnore finds the governing ignore file the way BuildKit does:
// an explicit --dockerignore wins; otherwise <Dockerfile-name>.dockerignore
// in the context root takes precedence over plain .dockerignore.
func resolveIgnore(bf *buildFlags, contextDir, dfPath string) (*ignore.Matcher, cachekey.IgnoreInfo, error) {
	var candidates []string
	if bf.dockerignore != "" {
		candidates = []string{bf.dockerignore}
	} else {
		candidates = []string{
			filepath.Join(contextDir, filepath.Base(dfPath)+".dockerignore"),
			filepath.Join(contextDir, ".dockerignore"),
		}
	}
	for _, cand := range candidates {
		data, err := os.ReadFile(cand)
		if err != nil {
			if os.IsNotExist(err) && bf.dockerignore == "" {
				continue
			}
			return nil, cachekey.IgnoreInfo{}, fmt.Errorf("dockerignore: %w", err)
		}
		m, err := ignore.Parse(strings.NewReader(string(data)))
		if err != nil {
			return nil, cachekey.IgnoreInfo{}, fmt.Errorf("%s: %w", cand, err)
		}
		sum := sha256.Sum256(data)
		info := cachekey.IgnoreInfo{
			Present:  true,
			Source:   filepath.Base(cand),
			Digest:   "sha256:" + hex.EncodeToString(sum[:]),
			Patterns: m.Patterns(),
		}
		return m, info, nil
	}
	return nil, cachekey.IgnoreInfo{}, nil
}

// insideContext returns p relative to the context when p points inside it.
func insideContext(contextDir, p string) (string, bool) {
	if p == "" {
		return "", false
	}
	absCtx, err1 := filepath.Abs(contextDir)
	absP, err2 := filepath.Abs(p)
	if err1 != nil || err2 != nil {
		return "", false
	}
	rel, err := filepath.Rel(absCtx, absP)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// onePath extracts the optional single positional context argument.
func onePath(rest []string, stderr io.Writer) (string, int) {
	switch len(rest) {
	case 0:
		return ".", ExitOK
	case 1:
		return rest[0], ExitOK
	default:
		fmt.Fprintf(stderr, "buildbust: expected at most one context argument, got %d\n", len(rest))
		return "", ExitUsage
	}
}

func runSnapshot(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var bf buildFlags
	bf.register(fs)
	out := fs.String("o", "", "snapshot output path (default: <context>/"+DefaultSnapshotName+")")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	contextDir, code := onePath(fs.Args(), stderr)
	if code != ExitOK {
		return code
	}
	snapPath := *out
	if snapPath == "" {
		snapPath = filepath.Join(contextDir, DefaultSnapshotName)
	}
	res, err := load(&bf, contextDir, snapPath)
	if err != nil {
		fmt.Fprintf(stderr, "buildbust: %v\n", err)
		return ExitRuntime
	}
	snap := snapshot.New(res.plan, res.dockerfilePath, time.Now())
	if err := snapshot.Write(snapPath, snap); err != nil {
		fmt.Fprintf(stderr, "buildbust: %v\n", err)
		return ExitRuntime
	}
	fmt.Fprintf(stdout, "snapshot written → %s (%d steps, %d stages, %d context files hashed)\n",
		snapPath, len(res.plan.Steps), len(res.plan.Stages), res.contextFiles)
	return ExitOK
}

func runExplain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var bf buildFlags
	bf.register(fs)
	against := fs.String("against", "", "snapshot to compare against (default: <context>/"+DefaultSnapshotName+")")
	format := fs.String("format", "text", "output format: text or json")
	update := fs.Bool("update", false, "rewrite the snapshot after explaining, making this run the new baseline")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	contextDir, code := onePath(fs.Args(), stderr)
	if code != ExitOK {
		return code
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "buildbust: unknown --format %q (want text or json)\n", *format)
		return ExitUsage
	}
	snapPath := *against
	if snapPath == "" {
		snapPath = filepath.Join(contextDir, DefaultSnapshotName)
	}
	snap, err := snapshot.Read(snapPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(stderr, "buildbust: no snapshot at %s — run `buildbust snapshot` first to record a baseline\n", snapPath)
			return ExitRuntime
		}
		fmt.Fprintf(stderr, "buildbust: %v\n", err)
		return ExitRuntime
	}
	res, err := load(&bf, contextDir, snapPath)
	if err != nil {
		fmt.Fprintf(stderr, "buildbust: %v\n", err)
		return ExitRuntime
	}
	result := diff.Compare(snap, res.plan)
	meta := render.Meta{
		Dockerfile:   res.dockerfilePath,
		Context:      contextDir,
		SnapshotPath: snapPath,
		SnapshotAt:   snap.CreatedAt,
	}
	if *format == "json" {
		if err := render.ExplainJSON(stdout, result, meta); err != nil {
			fmt.Fprintf(stderr, "buildbust: %v\n", err)
			return ExitRuntime
		}
	} else {
		render.ExplainText(stdout, result, meta)
	}
	if *update {
		fresh := snapshot.New(res.plan, res.dockerfilePath, time.Now())
		if err := snapshot.Write(snapPath, fresh); err != nil {
			fmt.Fprintf(stderr, "buildbust: %v\n", err)
			return ExitRuntime
		}
	}
	if result.Busted {
		return ExitBusted
	}
	return ExitOK
}

func runFiles(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("files", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var bf buildFlags
	bf.register(fs)
	format := fs.String("format", "text", "output format: text or json")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	contextDir, code := onePath(fs.Args(), stderr)
	if code != ExitOK {
		return code
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "buildbust: unknown --format %q (want text or json)\n", *format)
		return ExitUsage
	}
	// Exclude a default-named snapshot from the inventory here too, so
	// `files` and `explain` agree about what the context contains.
	res, err := load(&bf, contextDir, filepath.Join(contextDir, DefaultSnapshotName))
	if err != nil {
		fmt.Fprintf(stderr, "buildbust: %v\n", err)
		return ExitRuntime
	}
	meta := render.Meta{Dockerfile: res.dockerfilePath, Context: contextDir}
	if *format == "json" {
		if err := render.FilesJSON(stdout, res.plan, meta); err != nil {
			fmt.Fprintf(stderr, "buildbust: %v\n", err)
			return ExitRuntime
		}
		return ExitOK
	}
	render.FilesText(stdout, res.plan, meta)
	return ExitOK
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `buildbust %s — which file or Dockerfile line busted your build cache?

Usage:
  buildbust snapshot [flags] [context]   record the cache baseline (default: <context>/%s)
  buildbust explain  [flags] [context]   compare against the baseline; exit 1 when busted
  buildbust files    [flags] [context]   show which context files feed each COPY/ADD key
  buildbust version                      print the version

Shared flags:
  -f, --file PATH        Dockerfile path (default: <context>/Dockerfile)
  --dockerignore PATH    ignore file (default: auto-detected like BuildKit)
  --build-arg NAME=val   build-time variable (repeatable)

Snapshot flags:
  -o PATH                snapshot output path

Explain flags:
  --against PATH         snapshot to compare against
  --update               rewrite the snapshot after explaining

Explain / files flags:
  --format FORMAT        text (default) or json

Exit codes: 0 cache intact · 1 cache busted · 2 usage error · 3 runtime error
`, version.Version, DefaultSnapshotName)
}
