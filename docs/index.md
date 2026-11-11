# FluxVale Provider

The FluxVale provider deploys and manages [FluxVale](https://fluxvale.com) catalog apps as code — a thin skin over the FluxVale JSON:API (`/api/v1`, personal-access-token auth). Business rules (paywall, scopes, allocation bounds) enforce server-side; the provider re-implements nothing. Server errors (400 attribute/guard refusals, 403 scope/policy, 404) surface verbatim, never pre-empted client-side.

## Example Usage

```hcl
terraform {
  required_providers {
    fluxvale = {
      source = "registry.terraform.io/fluxvale/fluxvale"
    }
  }
}

provider "fluxvale" {
  # api_url = "https://staging.fluxvale.com"  # staging override; default https://fluxvale.com
}

resource "fluxvale_instance" "kavita" {
  name        = "Kavita"
  app         = "kavita"
  app_version = "3.4.1" # pin explicitly — the catalog's row order is not a release order
  cpu_cores   = 1
  memory_mb   = 512
  storage_gb  = 10
}
```

## Argument Reference (the provider block)

- **`api_url`** (Optional) — the FluxVale API base URL. Default `https://fluxvale.com`. An unknown value at plan time is an error, not a silent production default — staging setups must not silently send credentials to production.
- **`token`** (Optional, Sensitive) — a FluxVale personal access token (`fvpat_…`). Falls back to the `FLUXVALE_PAT` environment variable (preferred — no secrets in code).
- **`organization`** (Optional) — default organization for resources that need one, by id or slug. A resource's own `organization` attribute wins; with neither set, resources fall back to your sole organization (several memberships without a name is an error, never a silent pick).

Configure pings `/api/v1/me` (scopeless — any live token works), so a dead token fails at plan time with the server's own wording.

## Resources and data sources

Resources:

- [`fluxvale_instance`](resources/instance.md) — deploy a catalog app version
- [`fluxvale_instance_backups`](resources/instance_backups.md) — an instance's backup destination (R2)
- [`fluxvale_access_gateway`](resources/access_gateway.md) — the org's private-access gateway
- [`fluxvale_custom_domain`](resources/custom_domain.md) — your own domain on the private-access edge

Data sources:

- [`fluxvale_apps`](data-sources/apps.md) — the catalog
- [`fluxvale_app_versions`](data-sources/app_versions.md) — one app's versions

Guides: [Installing the provider](guides/install.md) · [Compatibility contract](guides/compatibility.md) · [The owner stack example](https://github.com/fluxvale/terraform-provider-fluxvale/tree/main/examples/owner-stack) · [The personal-stack example](https://github.com/fluxvale/terraform-provider-fluxvale/tree/main/examples/personal-stack)

## 0.x compatibility

The provider follows the platform's API, and 0.x may still adjust computed fields and add attributes. The [compatibility contract](guides/compatibility.md) states which names are stable from day one: the provider configuration, `FLUXVALE_PAT`, and every resource and data source name above. Config you write against those names keeps working.
