#!/usr/bin/env bash
# Wraps a docker build with a culprit report: explains what busted the
# cache since the last build, then re-baselines so the next run compares
# against this one. Drop it in front of your usual build command.
#
# Usage: bash examples/pre-build-report.sh <context> [docker build args...]
set -euo pipefail

CONTEXT="${1:?usage: pre-build-report.sh <context> [docker build args...]}"
shift || true

if ! command -v buildbust >/dev/null 2>&1; then
  echo "buildbust not on PATH — install it from a clone with: go install ./cmd/buildbust" >&2
  exit 1
fi

SNAP="$CONTEXT/.buildbust.json"
if [ ! -f "$SNAP" ]; then
  echo "no baseline yet — recording one"
  buildbust snapshot "$CONTEXT"
else
  # Exit code 1 means "busted", which is exactly the interesting case;
  # --update re-baselines either way. Real errors (exit >1) still abort.
  rc=0
  buildbust explain --update "$CONTEXT" || rc=$?
  if [ "$rc" -gt 1 ]; then exit "$rc"; fi
fi

# Hand off to the real build. Swap in `docker buildx build` if you use it.
echo "+ docker build $* $CONTEXT"
# docker build "$@" "$CONTEXT"
echo "(docker invocation commented out so this example stays offline)"
