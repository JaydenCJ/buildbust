# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-07-12

### Added

- Dockerfile parser covering parser directives (`# escape=`), line
  continuations with comment/blank elision, BuildKit heredocs
  (`<<EOF`, `<<-EOF`, quoted delimiters), JSON and shell forms,
  instruction flags (`--from`, `--exclude`, `--mount`, …), ARG/ENV
  key-value grammar, and multi-stage structure.
- Offline cache-key model: per-instruction sha256 keys from resolved
  text, per-file content+mode digests for COPY/ADD, and the stage ARG
  environment for RUN — matching the builder's documented invalidation
  rules (docs/cache-model.md, including honest divergences).
- Build-context scanner with full `.dockerignore` semantics (last match
  wins, `!` re-inclusion, `**`, directory coverage) and BuildKit's
  `<Dockerfile-name>.dockerignore` precedence; the snapshot file is
  self-excluded so buildbust never blames itself.
- `snapshot` subcommand recording the baseline as versioned, git-friendly
  JSON (`schema_version: 1`).
- `explain` subcommand naming the exact culprit step, line, and file
  (modified / added / removed / mode-changed, with digests), build-arg
  evidence, a per-stage blast radius with cross-stage `COPY --from`
  edges, independent extra busts, and a `.dockerignore`-edit suspect
  flag; `--format json`, `--update` re-baselining, exit code 1 on bust.
- `files` subcommand showing exactly which context files feed each
  COPY/ADD cache key, in text and JSON.
- Variable expansion (`$V`, `${V}`, `${V:-def}`, `${V:+alt}`) with
  Docker's ARG scoping: pre-FROM args reach FROM lines only, bare
  `ARG NAME` re-imports, ENV shadows ARG.
- Runnable examples (`examples/make-demo-context.sh`,
  `examples/pre-build-report.sh`) and a cache-model reference.
- 90 deterministic offline tests (unit + in-process CLI integration on
  fabricated build contexts) and `scripts/smoke.sh`.

[0.1.0]: https://github.com/JaydenCJ/buildbust/releases/tag/v0.1.0
