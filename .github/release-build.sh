#!/usr/bin/env bash
# Builds the archives a release publishes, plus the SHA256SUMS that identify
# them.
#
#   .github/release-build.sh v0.1.0 dist
#
# The version is an argument rather than a constant in the source: cmd/vacmcp
# holds "0.0.0-dev" and the tag is written into the binary at link time, so a
# developer build says it is a developer build and a release build says which
# release it is. Nothing here reads the git tag itself — the release workflow
# passes it in, which is what lets this script be run by hand against a test
# version without tagging anything.
#
# It is a script and not twenty lines of workflow YAML for the same reason the
# other .github scripts are: a release pipeline that can only be exercised by
# pushing a tag is a pipeline nobody tests.
set -euo pipefail

version=${1:-}
dist=${2:-dist}

if [ -z "$version" ]; then
	echo "usage: $0 <version> [output directory]" >&2
	exit 1
fi

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

# The platforms doc-1 §17 lists. Windows gets a zip because that is what a
# Windows user can open without installing anything.
platforms="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64"

# Apache-2.0 §4 asks for the license and the attribution notice to travel with
# the binary, so both are in every archive.
extras="LICENSE THIRD_PARTY_NOTICES.md README.md"

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$@"
	else
		shasum -a 256 "$@"
	fi
}

rm -rf "$dist"
mkdir -p "$dist"
# Absolute from here on: the builds run from the module root, and zip writes
# relative to whichever staging directory it was pointed at.
dist=$(cd "$dist" && pwd)

host_os=$(go env GOOS)
host_arch=$(go env GOARCH)
verified=

for platform in $platforms; do
	os=${platform%/*}
	arch=${platform#*/}

	stage="$dist/stage/${os}_${arch}"
	mkdir -p "$stage"

	binary="$stage/vacmcp"
	if [ "$os" = windows ]; then
		binary="$stage/vacmcp.exe"
	fi

	# -trimpath keeps the build directory out of the binary, so the archive does
	# not depend on where it was built. CGO is off: these are static binaries,
	# and a release build must not pick up whichever libc the runner happens to
	# have.
	echo "building $os/$arch"
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
		go build -trimpath -ldflags "-X main.version=$version" -o "$binary" ./cmd/vacmcp

	cp $extras "$stage/"

	archive="vacmcp_${version}_${os}_${arch}"
	if [ "$os" = windows ]; then
		(cd "$stage" && zip -q -r "$dist/$archive.zip" .)
	else
		tar -czf "$dist/$archive.tar.gz" -C "$stage" .
	fi

	# The one platform this machine can run is run: a release whose binary
	# reports 0.0.0-dev is a release nobody can identify afterwards, and the
	# only way to know the -X path still names a real variable is to ask the
	# binary. CI builds linux/amd64 on a linux/amd64 runner, so this always
	# fires there.
	if [ "$os" = "$host_os" ] && [ "$arch" = "$host_arch" ]; then
		reported=$("$binary" version)
		if [ "$reported" != "$version" ]; then
			echo "release-build: $os/$arch reports version '$reported', want '$version'." >&2
			echo "  The -X path no longer names the version variable in cmd/vacmcp." >&2
			exit 1
		fi
		verified="$os/$arch"
	fi
done

rm -rf "$dist/stage"

# Relative names, so `sha256sum -c SHA256SUMS` works for whoever downloads them.
(cd "$dist" && sha256 vacmcp_* >SHA256SUMS)

echo
echo "release-build: $version"
if [ -n "$verified" ]; then
	echo "  version injection verified by running the $verified binary"
else
	echo "  version injection NOT verified: no archive matches this host ($host_os/$host_arch)"
fi
cat "$dist/SHA256SUMS"
