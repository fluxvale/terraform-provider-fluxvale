# AGENTS.md

Working agreements for this repo — the Terraform/OpenTofu provider
for FluxVale, extracted from the platform monorepo
([ADR-0019](https://github.com/fluxvale/fluxvale/blob/main/docs/adr/00019-machine-first-api-cli-mcp.md)
Am. 4; the extraction itself has its own ADR in the monorepo's
`docs/adr/` index). The monorepo's root AGENTS.md carries the org-wide
conventions (worktrees, PR lifecycle, review budget); everything here
is repo-specific.

## Toolchain

- `mise install` — exact-pinned Go + OpenTofu (`mise.toml` +
  `mise.lock`); bumps are deliberate, reviewable diffs.
- No Node, no BEAM — this repo is Go only.

## The gate

Before every PR, the same command CI runs (`.github/workflows/ci.yml`):

```sh
gofmt -l . && go vet ./... && go test ./... && go build ./...
./scripts/test-check-tag-version.sh && shellcheck scripts/*.sh
mise x -- tofu fmt -check -recursive examples
TFPROVIDER_E2E=1 mise x -- go test ./internal/provider -run TestRealTofuSmoke -count=1 -v
```

## Releases

- Tags are plain `vX.Y.Z` (no prefix — the registries list semver).
  `internal/provider/version.go`'s const must equal the tag;
  `scripts/check-tag-version.sh` fails a release that disagrees.
- The bump carries its doc sweep: `docs/guides/install.md` and the
  platform monorepo's `docs/public/automation/terraform.md` name the
  concrete archives.
- The release workflow packs `terraform-provider-registry-manifest.json`
  into every zip and GPG-signs `SHA256SUMS` with the company key
  (public key committed at `terraform-provider-signing.asc`; the
  private half lives in 1Password and in the `GPG_PRIVATE_KEY`
  Actions secret).

## Compatibility contract

`docs/guides/compatibility.md` is binding: provider config attribute
names, `FLUXVALE_PAT`, and resource/data-source type names are stable
from day one — renaming any of them is a breaking change, never done
silently in 0.x.
