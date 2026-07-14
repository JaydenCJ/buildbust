#!/usr/bin/env bash
# End-to-end smoke test for buildbust: builds the binary, fabricates a
# deterministic two-stage build context, and asserts on the real CLI
# output across snapshot → explain → files. No network, idempotent,
# finishes in seconds.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

fail() {
  echo "SMOKE FAIL: $*" >&2
  exit 1
}

BIN="$WORKDIR/buildbust"
CTX="$WORKDIR/ctx"

echo "1. build"
(cd "$ROOT" && go build -o "$BIN" ./cmd/buildbust) || fail "go build failed"

echo "2. version matches manifest"
"$BIN" --version | grep -qx "buildbust 0.1.0" || fail "--version mismatch"

echo "3. fabricate a build context"
bash "$ROOT/examples/make-demo-context.sh" "$CTX" >/dev/null || fail "demo context script failed"

echo "4. snapshot records the baseline"
OUT="$("$BIN" snapshot "$CTX")"
echo "$OUT" | grep -q "snapshot written" || fail "snapshot summary missing"
echo "$OUT" | grep -q "11 steps, 3 stages" || fail "step/stage counts wrong: $OUT"
[ -f "$CTX/.buildbust.json" ] || fail "snapshot file not created"

echo "5. clean tree explains as CACHE OK (exit 0)"
"$BIN" explain "$CTX" | grep -q "CACHE OK" || fail "expected CACHE OK"

echo "6. editing a source file pins the exact file and line"
printf 'exports.serve = (port) => console.log("listening on 127.0.0.1:" + port); // v2\n' \
  > "$CTX/src/lib/util.js"
set +e
OUT="$("$BIN" explain "$CTX")"
CODE=$?
set -e
[ "$CODE" -eq 1 ] || fail "busted cache must exit 1, got $CODE"
echo "$OUT" | grep -q "CACHE BUSTED at step 7/11" || fail "wrong culprit step: $OUT"
echo "$OUT" | grep -q "COPY src/ ./src/" || fail "culprit instruction missing"
echo "$OUT" | grep -q "src/lib/util.js" || fail "culprit file missing"
echo "$OUT" | grep -q "~ modified" || fail "change kind missing"
echo "$OUT" | grep -q "via COPY --from=build" || fail "cross-stage blast radius missing"

echo "7. JSON output is machine-readable"
set +e
JSON="$("$BIN" explain --format json "$CTX")"
set -e
echo "$JSON" | grep -q '"tool": "buildbust"' || fail "json envelope missing"
echo "$JSON" | grep -q '"busted": true' || fail "json verdict missing"
echo "$JSON" | grep -q '"path": "src/lib/util.js"' || fail "json culprit file missing"

echo "8. --update re-baselines"
set +e
"$BIN" explain --update "$CTX" >/dev/null
set -e
"$BIN" explain "$CTX" | grep -q "CACHE OK" || fail "--update did not re-baseline"

echo "9. build-arg change blames the consuming RUN"
set +e
OUT="$("$BIN" explain --build-arg APP_ENV=staging "$CTX")"
CODE=$?
set -e
[ "$CODE" -eq 1 ] || fail "arg change must exit 1"
echo "$OUT" | grep -q 'APP_ENV: "production" → "staging"' || fail "arg evidence missing: $OUT"

echo "10. files shows per-instruction context inventory"
OUT="$("$BIN" files "$CTX")"
echo "$OUT" | grep -q "COPY package.json package-lock.json ./" || fail "files step missing"
echo "$OUT" | grep -q "package-lock.json" || fail "files inventory missing"
if echo "$OUT" | grep -q "README.md"; then
  fail ".dockerignore not applied in files output"
fi

echo "11. usage errors exit 2"
set +e
"$BIN" explain --format yaml "$CTX" >/dev/null 2>&1
[ $? -eq 2 ] || fail "bad --format should exit 2"
"$BIN" bogus-command >/dev/null 2>&1
[ $? -eq 2 ] || fail "unknown command should exit 2"
set -e

echo "SMOKE OK"
