# FluxVale Terraform/OpenTofu provider

A thin skin over the FluxVale JSON:API (`/api/v1`, PAT auth) —
[ADR-0019](../../docs/adr/00019-machine-first-api-cli-mcp.md) Am. 4.
Policy, paywall, and scopes enforce server-side; the provider
re-implements nothing.

**Status**: scaffold (#482 P3). Provider config + API client only —
resources and data sources land in follow-up slices. The v0 resource
set is named in the [compatibility contract](docs/compatibility.md).

## Authentication

The provider needs a FluxVale personal access token (`fvpat_…`):

- `token` attribute in the provider block, or
- the `FLUXVALE_PAT` environment variable (preferred — no secrets in
  code; placeholders only in `.tf` files).

Configure pings `/api/v1/me` (scopeless — any live token works), so a
dead token fails at plan time with the server's wording.

## Dev loop (dev_overrides)

The registry isn't live yet; use [dev_overrides](https://developer.hashicorp.com/terraform/plugin/debug-vendor):

```sh
cd apps/tfprovider && go build -o ~/.tfplugins/terraform-provider-fluxvale .
```

`~/.terraformrc` (OpenTofu: `~/.tofurc`):

```hcl
provider_installation {
  dev_overrides {
    "registry.terraform.io/fluxvale/fluxvale" = "/home/you/.tfplugins"
  }
  direct {}
}
```

Then point a config at the provider — no `terraform init` needed:

```hcl
terraform {
  required_providers {
    fluxvale = { source = "registry.terraform.io/fluxvale/fluxvale" }
  }
}

provider "fluxvale" {
  # api_url = "https://staging.fluxvale.com"  # staging override
}
```

Or run it debugger-attached: `go run . -debug` and follow the printed
`TF_REATTACH_PROVIDERS` export.

## CI

Path-gated lane in `.github/workflows/ci.yml` (`tfprovider`): gofmt +
vet + test + build, Go version parsed from the root `mise.toml` pin.
