#!/bin/sh
# Fetches the libraries the interface is built on, at the versions pinned
# below, and writes them into this folder as plain ES modules.
#
#   web/lib/update.sh          (needs wget, tar and sha512sum: any Linux)
#
# The files are published builds, unchanged but for two things: the line that
# points at a source map is dropped, and imports by package name ("preact")
# are rewritten to the neighbouring file ("./preact.js"), because a browser
# cannot resolve a package name without a bundler or an import map.
#
# To move to a newer version: change the version and checksum here, run this,
# run the tests (scripts/e2e.sh drives a real browser), and commit the result.
set -eu
cd "$(dirname "$0")"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# fetch <tarball url> <sha512 of the tarball, hex> <file in package> <name here> <licence name here>
fetch() {
	tgz="$tmp/$(basename "$1")"
	wget -q -O "$tgz" "$1"
	echo "$2  $tgz" | sha512sum -c - >/dev/null || { echo "checksum mismatch: $1" >&2; exit 1; }
	mkdir -p "$tgz.d"
	tar xzf "$tgz" -C "$tgz.d"
	sed -e '/^\/\/# sourceMappingURL=/d' \
		-e 's|from"preact/hooks"|from"./hooks.js"|g' \
		-e 's|from"preact"|from"./preact.js"|g' \
		-e 's|from"@preact/signals-core"|from"./signals-core.js"|g' \
		"$tgz.d/package/$3" > "$4"
	cp "$tgz.d/package/LICENSE" "$5"
}

npm=https://registry.npmjs.org
preact=$npm/preact/-/preact-11.0.0.tgz
preact_sum=2d14cb2ae49ee9560f86f0438f015a5fe9ee667a6cc3a20dd8649ba22b00be11ac9dcca5066adb0c21118602839fac8fda60306c6bec079395c937b7dccf2006

fetch "$preact" $preact_sum dist/preact.mjs preact.js LICENSE-preact
fetch "$preact" $preact_sum hooks/dist/hooks.mjs hooks.js LICENSE-preact
fetch $npm/htm/-/htm-3.1.1.tgz \
	f7cdd5ca0f0dc1413b26467a3663aaa4267eb21d5b2afda2613954933956980e490f669c2a8c5de0a5716cc9b15fff399ad7dd9c399316834a720e43180bf135 \
	dist/htm.mjs htm.js LICENSE-htm
fetch $npm/@preact/signals-core/-/signals-core-1.14.4.tgz \
	1cd07a1d87982a141b27568a97e6118eb4b8f905872ca5faa8aa14b1f4bf9b4beacec55a10189989a29b1bf7be34a936721e402d0aff8a159a307c620aeb961c \
	dist/signals-core.mjs signals-core.js LICENSE-signals
fetch $npm/@preact/signals/-/signals-2.11.3.tgz \
	9adb7014cf7c1bf262ae24e033053e516ca02fab1f56660488d70f48a9149efe0fc4fa598663f6ba0182e2a8b7eb4f99207c90f0d634e5b7c94f76f845420697 \
	dist/signals.mjs signals.js LICENSE-signals

# Nothing may still be imported by package name.
if grep -o 'from"[^./][^"]*"' ./*.js; then
	echo "an import by package name is left in the files above" >&2
	exit 1
fi
sha256sum ./*.js > CHECKSUMS
cat CHECKSUMS
