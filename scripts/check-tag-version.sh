#!/usr/bin/env bash
# The release-tag gate (mirrors apps/cli/scripts/check-tag-version.sh):
# a `tf-v*` tag must carry exactly the version in
# internal/provider/version.go — the provider's version source (const
# Version, wired into the User-Agent and the -version flag). A tag that
# disagrees with the shipped binary's self-report fails the release, so
# lazy tags can't ship.
#
# Usage: check-tag-version.sh tf-vX.Y.Z [version-go-path]
#   The second arg overrides the version.go location — the test seam
#   for the no-version branch; the release workflow passes none.
# Exit 0 on match; 1 on mismatch, a foreign tag, or no version at all.
set -euo pipefail

tag="${1:?usage: check-tag-version.sh tf-vX.Y.Z [version-go-path]}"

case "$tag" in
  tf-v*) version="${tag#tf-v}" ;;
  *)
    echo "not a tfprovider release tag: '$tag' (want tf-v<version>)" >&2
    exit 1
    ;;
esac

version_go="${2:-$(cd "$(dirname "$0")/.." && pwd)/internal/provider/version.go}"
# The only `Version = "..."` const in the file is the release version —
# the comment above it never quotes another assignment.
const_version="$(sed -n 's/^const Version = "\([^"]*\)"/\1/p' "$version_go" | head -1)"

if [ -z "$const_version" ]; then
  echo "no const Version found in $version_go" >&2
  exit 1
fi

if [ "$version" = "$const_version" ]; then
  echo "tag $tag matches version.go version $const_version"
else
  echo "tag $tag does not match the provider's version: version.go says $const_version — bump version.go (or re-tag) so the release name and \`terraform-provider-fluxvale -version\` agree" >&2
  exit 1
fi
