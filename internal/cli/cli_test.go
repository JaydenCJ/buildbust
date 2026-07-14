// In-process CLI integration tests: real temp-dir build contexts, real
// snapshot files, real exit codes — everything a user script would see,
// without building a binary or touching the network.
package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JaydenCJ/buildbust/internal/version"
)

// run invokes the CLI in-process and captures stdout/stderr.
func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// demoContext builds a two-stage app context in a temp dir.
func demoContext(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Dockerfile", `FROM golang:1.22 AS build
ARG LDFLAGS=-s
COPY go.mod ./
RUN go mod download
COPY src/ ./src/
RUN go build -ldflags "$LDFLAGS" -o /bin/app ./src
FROM alpine:3.20
COPY --from=build /bin/app /usr/local/bin/app
CMD ["app"]
`)
	write("go.mod", "module demo\n\ngo 1.22\n")
	write("src/main.go", "package main\n\nfunc main() {}\n")
	write("src/server.go", "package main\n\nfunc serve() {}\n")
	write("notes.md", "scratch notes\n")
	write(".dockerignore", "*.md\n")
	return dir
}

func TestSnapshotThenExplainCleanExitsZero(t *testing.T) {
	dir := demoContext(t)
	code, out, errOut := run(t, "snapshot", dir)
	if code != ExitOK {
		t.Fatalf("snapshot exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "9 steps, 2 stages") {
		t.Fatalf("summary = %q", out)
	}
	code, out, _ = run(t, "explain", dir)
	if code != ExitOK {
		t.Fatalf("explain exit=%d out=%s", code, out)
	}
	if !strings.Contains(out, "CACHE OK") {
		t.Fatalf("out = %q", out)
	}
}

func TestExplainAfterEditNamesFileAndExitsOne(t *testing.T) {
	dir := demoContext(t)
	if code, _, e := run(t, "snapshot", dir); code != ExitOK {
		t.Fatalf("snapshot: %s", e)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "server.go"), []byte("package main // edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "explain", dir)
	if code != ExitBusted {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitBusted, out)
	}
	for _, want := range []string{"CACHE BUSTED at step 5/9", "line 5", "src/server.go", "~ modified"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestExplainJSONFormat(t *testing.T) {
	dir := demoContext(t)
	run(t, "snapshot", dir)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module demo2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "explain", "--format", "json", dir)
	if code != ExitBusted {
		t.Fatalf("exit = %d", code)
	}
	var parsed struct {
		Tool    string `json:"tool"`
		Busted  bool   `json:"busted"`
		Culprit struct {
			Step struct {
				Line int `json:"line"`
			} `json:"step"`
			FileChanges []struct {
				Path string `json:"path"`
			} `json:"file_changes"`
		} `json:"culprit"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out)
	}
	if parsed.Tool != "buildbust" || !parsed.Busted || parsed.Culprit.Step.Line != 3 {
		t.Fatalf("parsed = %+v", parsed)
	}
	if parsed.Culprit.FileChanges[0].Path != "go.mod" {
		t.Fatalf("file = %+v", parsed.Culprit.FileChanges)
	}
}

func TestRuntimeErrorsExitThree(t *testing.T) {
	// explain without a baseline points the user at `buildbust snapshot`.
	dir := demoContext(t)
	code, _, errOut := run(t, "explain", dir)
	if code != ExitRuntime {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(errOut, "buildbust snapshot") {
		t.Fatalf("stderr = %q (must tell the user how to record a baseline)", errOut)
	}
	// A broken Dockerfile surfaces the parse error with the same exit code.
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "Dockerfile"), []byte("RUN before-from\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut = run(t, "snapshot", broken)
	if code != ExitRuntime {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(errOut, "FROM") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestExplainUpdateMakesNextRunClean(t *testing.T) {
	dir := demoContext(t)
	run(t, "snapshot", dir)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module demo2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, _ := run(t, "explain", "--update", dir)
	if code != ExitBusted {
		t.Fatalf("first explain exit = %d", code)
	}
	code, out, _ := run(t, "explain", dir)
	if code != ExitOK || !strings.Contains(out, "CACHE OK") {
		t.Fatalf("after --update: exit=%d out=%s", code, out)
	}
}

func TestBuildArgChangeBlamesFirstRunConsumer(t *testing.T) {
	dir := demoContext(t)
	run(t, "snapshot", "--build-arg", "LDFLAGS=-s", dir)
	code, out, _ := run(t, "explain", "--build-arg", "LDFLAGS=-s -w", dir)
	if code != ExitBusted {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if !strings.Contains(out, "CACHE BUSTED at step 4/9") {
		t.Fatalf("expected step 4 (first RUN in scope):\n%s", out)
	}
	if !strings.Contains(out, `~ LDFLAGS: "-s" → "-s -w"`) {
		t.Fatalf("arg evidence missing:\n%s", out)
	}
}

func TestSnapshotFileIsInvisibleToItself(t *testing.T) {
	// The default snapshot lives inside the context; a naive scan would
	// report it as an added file on the next explain of a COPY . step.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine\nCOPY . /app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "snapshot", dir)
	code, out, _ := run(t, "explain", dir)
	if code != ExitOK {
		t.Fatalf("exit=%d out=%s (snapshot file leaked into its own context)", code, out)
	}
}

func TestDockerignoreEditIsCalledOut(t *testing.T) {
	dir := demoContext(t)
	run(t, "snapshot", dir)
	// Dropping the *.md rule pulls notes.md into COPY . …-free contexts;
	// here no COPY reads it, so the report stays OK but notes the edit.
	if err := os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte("# nothing ignored now\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "explain", dir)
	if code != ExitOK {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if !strings.Contains(out, ".dockerignore changed") {
		t.Fatalf("ignore-change note missing:\n%s", out)
	}
}

func TestCustomDockerfilePathAndSnapshotOutput(t *testing.T) {
	dir := demoContext(t)
	alt := filepath.Join(dir, "build", "Dockerfile.prod")
	if err := os.MkdirAll(filepath.Dir(alt), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alt, []byte("FROM alpine\nCOPY go.mod /go.mod\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(t.TempDir(), "prod.json")
	code, _, errOut := run(t, "snapshot", "-f", alt, "-o", snap, dir)
	if code != ExitOK {
		t.Fatalf("snapshot: %s", errOut)
	}
	code, out, _ := run(t, "explain", "-f", alt, "--against", snap, dir)
	if code != ExitOK || !strings.Contains(out, "CACHE OK") {
		t.Fatalf("exit=%d out=%s", code, out)
	}
}

func TestFilesCommandShowsInventory(t *testing.T) {
	dir := demoContext(t)
	code, out, _ := run(t, "files", dir)
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"COPY go.mod ./", "go.mod", "COPY src/ ./src/", "src/main.go", "src/server.go", `copies from stage "build"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// .dockerignore applies: notes.md must not appear anywhere.
	if strings.Contains(out, "notes.md") {
		t.Fatalf("ignored file leaked:\n%s", out)
	}
}

func TestFilesJSONIsParseable(t *testing.T) {
	dir := demoContext(t)
	code, out, _ := run(t, "files", "--format", "json", dir)
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var parsed struct {
		Steps []struct {
			Cmd   string `json:"cmd"`
			Files []struct {
				Path string `json:"path"`
			} `json:"files"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(parsed.Steps) != 3 { // two context COPYs + one --from COPY
		t.Fatalf("steps = %+v", parsed.Steps)
	}
}

func TestVersionAndHelp(t *testing.T) {
	code, out, _ := run(t, "version")
	if code != ExitOK || out != "buildbust "+version.Version+"\n" {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	code, out2, _ := run(t, "--version")
	if code != ExitOK || out2 != out {
		t.Fatal("--version must match the subcommand")
	}
	code, help, _ := run(t, "help")
	if code != ExitOK || !strings.Contains(help, "buildbust snapshot") || !strings.Contains(help, "Exit codes") {
		t.Fatalf("exit=%d out=%s", code, help)
	}
	code, bare, _ := run(t)
	if code != ExitOK || bare != help {
		t.Fatal("bare invocation must print the same usage")
	}
}

func TestUsageErrors(t *testing.T) {
	if code, _, _ := run(t, "unknown-cmd"); code != ExitUsage {
		t.Fatalf("unknown command exit = %d", code)
	}
	if code, _, _ := run(t, "explain", "--format", "yaml", "."); code != ExitUsage {
		t.Fatalf("bad format exit = %d", code)
	}
	if code, _, _ := run(t, "explain", "a", "b"); code != ExitUsage {
		t.Fatalf("two contexts exit = %d", code)
	}
	dir := demoContext(t)
	if code, _, _ := run(t, "snapshot", "--build-arg", "MISSING_EQUALS", dir); code != ExitRuntime {
		t.Fatalf("bad build-arg should fail loudly")
	}
}

func TestNamedDockerignoreTakesPrecedence(t *testing.T) {
	// BuildKit prefers <Dockerfile-name>.dockerignore over .dockerignore.
	dir := demoContext(t)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile.dockerignore"), []byte("src\n*.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "files", dir)
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(out, "src/main.go") {
		t.Fatalf("Dockerfile.dockerignore not honored:\n%s", out)
	}
	if !strings.Contains(out, "matched no files") {
		t.Fatalf("expected a matched-no-files note for COPY src/:\n%s", out)
	}
}
