# terraform-provider-fluxvale

The FluxVale Terraform/OpenTofu provider — a thin skin over the
FluxVale JSON:API (`/api/v1`, PAT auth). Policy, paywall, and scopes
enforce server-side; the provider re-implements nothing. Its 400s
(attribute/guard refusals), 403s (scope/policy), and 404s surface
verbatim, never pre-empted client-side.

Extracted from the [FluxVale platform monorepo](https://github.com/fluxvale/fluxvale)
(`apps/tfprovider` there — [ADR-0019](https://github.com/fluxvale/fluxvale/blob/main/docs/adr/00019-machine-first-api-cli-mcp.md)
Am. 4 and the extraction ADR beside it in the monorepo's index) so
both provider registries can list it: plain `vX.Y.Z` tags, GPG-signed
`SHA256SUMS`, and the registry manifest inside every release zip.

## Documentation

The registry-rendered docs live in [`docs/`](docs/index.md) — the
provider page, per-resource and per-data-source pages, and the
[install guide](docs/guides/install.md). Working examples:

- [`examples/owner-stack`](examples/owner-stack) — the owner's whole
  stack as one root module: R2 bucket, tailnet access gateway,
  instances, backups, custom domains
- [`examples/personal-stack`](examples/personal-stack) — the
  single-instance teaching example

## Releases

Tagged `vX.Y.Z` on this repo's [Releases](https://github.com/fluxvale/terraform-provider-fluxvale/releases):
four platform zips (linux amd64/arm64, darwin arm64, windows amd64)
carrying only the binary, plus the registry's versioned assets —
`terraform-provider-fluxvale_<version>_manifest.json` (protocol 6.0),
`_SHA256SUMS` covering the zips and the manifest, and the binary GPG
signature `_SHA256SUMS.sig`, verifiable against the committed
[`terraform-provider-signing.asc`](terraform-provider-signing.asc).

## Dev loop

`mise install` (Go + OpenTofu, pinned), then the gates CI runs:

```sh
gofmt -l . && go vet ./... && go test ./... && go build ./...
./scripts/test-check-tag-version.sh && shellcheck scripts/*.sh
mise x -- tofu fmt -check -recursive examples
TFPROVIDER_E2E=1 mise x -- go test ./internal/provider -run TestRealTofuSmoke -count=1 -v
```

The real-tofu smoke is offline (an httptest fake API +
`dev_overrides`); the acceptance lane (`.github/workflows/acceptance.yml`,
nightly) runs the same suite against a real org.

Debugger-attached: `go run . -debug` and follow the printed
`TF_REATTACH_PROVIDERS` export.
