#!/usr/bin/env bash
# Proves check-tag-version.sh on fixtures — the same shape
# apps/cli/scripts/test-check-tag-version.sh runs in the cli lane's
# gates. Run in the tfprovider lane (ci.yml) so the release gate is
# exercised on every provider-touching PR, not only at tag time.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
check="$here/check-tag-version.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

expect_ok() {
  local desc="$1"; shift
  if "$@"; then echo "ok: $desc"; else echo "FAIL (want exit 0): $desc"; exit 1; fi
}

expect_fail() {
  local desc="$1"; shift
  if "$@" 2>/dev/null; then echo "FAIL (want nonzero): $desc"; exit 1; else echo "ok: $desc"; fi
}

# The real version.go: the tag carrying its version passes; the same
# version under a foreign namespace is refused.
expect_ok "tf-v tag matching version.go" "$check" "tf-v$(sed -n 's/^const Version = "\([^"]*\)"/\1/p' "$here/../internal/provider/version.go")"
expect_fail "foreign namespace (cli-v) is refused" "$check" "cli-v0.1.0" "$here/../internal/provider/version.go"
expect_fail "wrong version fails" "$check" "tf-v9.9.9" "$here/../internal/provider/version.go"
expect_fail "bare version fails" "$check" "0.1.0" "$here/../internal/provider/version.go"

# The test seam: a version.go with no const must fail, not pass on an
# empty grep.
printf 'package provider\n' > "$scratch/version.go"
expect_fail "no const Version fails" "$check" "tf-v0.1.0" "$scratch/version.go"

echo "all check-tag-version fixtures passed"
