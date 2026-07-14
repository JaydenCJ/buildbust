# The buildbust cache model

buildbust never talks to a Docker daemon. It re-derives, offline and
deterministically, the decision the builder makes for every instruction:
*cached* or *rebuild*. This document specifies the model and its honest
divergences from BuildKit.

## Per-instruction keys

Every step gets a key: `sha256` over the instruction kind, its resolved
text, the digests of the context files it pulls in, and (for RUN) the
build-arg environment.

| Instruction | Key material |
|---|---|
| `FROM` | resolved image reference (variables from pre-FROM ARGs expanded) |
| `COPY` / `ADD` from context | resolved text + per-file `path`, `mode`, `content sha256`, `size` of every matched file |
| `COPY --from=<stage>` | resolved text; invalidated when the source stage rebuilds |
| `RUN` | raw text (the shell expands variables, not the builder) + all stage-declared ARG values + heredoc bodies |
| `ARG` | raw line text only — a changed `--build-arg` misses at the first consumer, not at the declaration |
| `ENV`, `WORKDIR`, `LABEL`, … | resolved text (variables expanded the way the builder expands them) |

File hashing matches the builder's checksums: content and permission bits
count, modification times do not. A `chmod +x` busts a COPY; `touch` does
not.

## Context scanning

The scan applies `.dockerignore` with Docker's semantics (last match wins,
`!` re-inclusion, `**`, directory coverage) and honors BuildKit's lookup
order: `<Dockerfile-name>.dockerignore` beats `.dockerignore`. The
snapshot file itself (`.buildbust.json` or the `-o`/`--against` path) is
always excluded so buildbust never reports itself as the culprit.

## ARG scoping

Pre-FROM ARGs are visible to `FROM` lines only; a stage must re-import
them with a bare `ARG NAME`. Inside a stage, `ENV` shadows `ARG` in
expansion. Every RUN key folds in all ARGs declared in its stage so far —
which is exactly why changing one `--build-arg` misses at the first RUN
after the declaration, the builder's documented behavior.

## Invalidation and blast radius

Within a stage, the first key miss invalidates every later step of that
stage. Across stages, any step that reads from another stage
(`FROM <stage>`, `COPY --from`, `RUN --mount=from=`) is invalidated when
its source stage rebuilds, from that step onward. buildbust reports the
first miss as **the culprit**, the per-stage rebuild spans as the **blast
radius**, and any independently changed steps outside that cascade under
**also changed**.

## Honest divergences from BuildKit

- **No registry digests.** `FROM alpine:3.20` is keyed by reference; a
  re-tagged upstream image is invisible offline. Pin digests if this
  matters to you.
- **Remote `ADD` sources** (`https://…`, `git@…`) are keyed by URL only,
  with a note in the report; the content is never fetched.
- **Cross-stage copies are conservative**: any rebuild in the source stage
  is treated as invalidating the dependent `COPY --from`, while BuildKit
  can still hit when the copied paths are byte-identical.
- **`${VAR#prefix}`-style BuildKit string manipulation** is rejected with
  a clear error rather than mis-keyed (`:-` and `:+` are supported).
- Predefined platform args (`TARGETPLATFORM`, proxy variables) are not
  auto-injected; declare them explicitly if your keys depend on them.
