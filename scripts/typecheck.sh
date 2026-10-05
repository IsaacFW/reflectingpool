#!/bin/sh
# Type-checks the interface's JavaScript against its JSDoc comments.
#
#   scripts/typecheck.sh
#
# Nothing is built: the browser runs the files as they are. This only reads
# them. It needs Docker, and fetches TypeScript and the libraries' type
# definitions from npm into a throwaway folder each time. The versions match
# the files in web/lib (see web/lib/update.sh).
set -e
repo=$(cd "$(dirname "$0")/.." && pwd)
exec docker run --rm -v "$repo/web":/web:ro node:24-alpine sh -c '
  set -e
  mkdir /check && cd /check
  npm install --silent --no-audit --no-fund \
    typescript@7.0.2 preact@11.0.0 htm@3.1.1 @preact/signals@2.11.3 @preact/signals-core@1.14.4 >/dev/null
  # The checker looks for packages beside the files it reads.
  cp -r /web /check/web && ln -s /check/node_modules /check/web/node_modules
  ./node_modules/.bin/tsc -p web'
