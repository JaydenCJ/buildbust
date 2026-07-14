#!/usr/bin/env bash
# Fabricates a small, deterministic Docker build context: a two-stage
# Node-style app with a .dockerignore, the shape most cache mysteries
# come in. Usage: bash examples/make-demo-context.sh /tmp/buildbust-demo
set -euo pipefail

DEST="${1:?usage: make-demo-context.sh <dir>}"
rm -rf "$DEST"
mkdir -p "$DEST/src/lib"

cat > "$DEST/Dockerfile" <<'DOCKERFILE'
FROM node:22-alpine AS deps
COPY package.json package-lock.json ./
RUN npm ci --omit=dev

FROM node:22-alpine AS build
ARG APP_ENV=production
COPY --from=deps /node_modules ./node_modules
COPY src/ ./src/
RUN npm run build

FROM node:22-alpine
COPY --from=build /dist /srv/app
CMD ["node", "/srv/app/server.js"]
DOCKERFILE

cat > "$DEST/.dockerignore" <<'IGNORE'
node_modules
*.log
*.md
.buildbust.json
IGNORE

printf '{\n  "name": "demo",\n  "version": "1.0.0"\n}\n' > "$DEST/package.json"
printf '{\n  "lockfileVersion": 3\n}\n' > "$DEST/package-lock.json"
printf 'const lib = require("./lib/util");\nmodule.exports = () => lib.serve(8080);\n' > "$DEST/src/server.js"
printf 'exports.serve = (port) => console.log("listening on 127.0.0.1:" + port);\n' > "$DEST/src/lib/util.js"
printf '# demo app\nScratch notes, excluded by .dockerignore.\n' > "$DEST/README.md"
printf 'debug output\n' > "$DEST/build.log"

echo "demo context written to $DEST"
echo "next: buildbust snapshot $DEST && buildbust explain $DEST"
