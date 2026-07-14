# Contributing to buildbust

Issues, discussions and pull requests are all welcome.

## Getting started

You need Go ≥1.22; nothing else — no Docker daemon, no network.

```bash
git clone https://github.com/JaydenCJ/buildbust && cd buildbust
go build ./...
go test ./...
bash scripts/smoke.sh
```

`scripts/smoke.sh` builds the binary, fabricates a deterministic
two-stage build context in a temp dir, and asserts on real CLI output
across snapshot → explain → files; it must finish by printing `SMOKE OK`.

## Before you open a pull request

1. `gofmt -l .` reports nothing (formatting is enforced).
2. `go vet ./...` passes with no findings.
3. `go test ./...` passes (90 deterministic tests, no network).
4. `bash scripts/smoke.sh` prints `SMOKE OK`.
5. Add tests for behavior changes; keep logic in pure, unit-testable
   modules (the parser, matcher, keyer and differ never touch the
   filesystem — only `contextscan` and the CLI do).

## Ground rules

- Keep dependencies at zero: buildbust is standard library only, and its
  whole value is being runnable anywhere a Dockerfile lives. No telemetry,
  no network calls, ever.
- Cache-key semantics are contract: any change to what feeds a key must
  update `docs/cache-model.md` and bump the snapshot `schema_version`,
  with a test reproducing the Docker behavior being modeled.
- Determinism first: identical context + Dockerfile + args must produce
  byte-identical reports, including all orderings.
- Code comments and doc comments are written in English.

## Reporting bugs

Include the output of `buildbust version`, the Dockerfile (redacted is
fine as long as instruction structure survives), the report output, and —
for wrong-culprit reports — the `explain --format json` envelope, since
it carries the exact keys and file digests the differ compared.

## Security

Please do not open public issues for security problems; use GitHub's
private vulnerability reporting on this repository instead.
