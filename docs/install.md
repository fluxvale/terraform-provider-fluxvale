# Installing the provider

Manual-first ([ADR-0019](../../../docs/adr/00019-machine-first-api-cli-mcp.md)
Am. 4): the registry isn't live yet, so the provider installs from
GitHub Releases into a local filesystem mirror — or, for dev work,
straight off a freshly built binary via `dev_overrides`.

## 1. Download a release

Grab the zip for your platform from the repo's
[Releases](https://github.com/fluxvale/fluxvale/releases) (tag `tf-v*`)
plus `SHA256SUMS` (one merged file covering all four platforms), and
verify YOUR line of it — a single-platform download can't check the
whole set:

```sh
grep <your-zip> SHA256SUMS | sha256sum -c -
# e.g. grep terraform-provider-fluxvale_0.1.1_linux_amd64.zip SHA256SUMS | sha256sum -c -
#      macOS: … | shasum -a 256 -c -
```

## 2. Lay out the filesystem mirror

The registry's **packed layout**: the zip sits directly in the type
directory — NO version directory (the version is in the zip's
filename), and the os/arch names are the registry's vocabulary:

```
~/.tfplugins-mirror/
  registry.terraform.io/
    fluxvale/
      fluxvale/
        terraform-provider-fluxvale_0.1.1_linux_amd64.zip
        terraform-provider-fluxvale_0.1.1_linux_arm64.zip
        terraform-provider-fluxvale_0.1.1_darwin_arm64.zip
        terraform-provider-fluxvale_0.1.1_windows_amd64.zip
```

Drop the downloaded zips in unchanged — this packed layout is
exercised for real by the smoke gate in CI
(`internal/provider/e2e_test.go` builds a packed mirror and runs tofu
init against it).

## 3. Point the CLI at it

`~/.tofurc` (Terraform: `~/.terraformrc`) — see
[the CLI configuration files docs](https://opentofu.org/docs/cli/config/files/)
for the full shape:

```hcl
provider_installation {
  filesystem_mirror {
    path    = "/home/you/.tfplugins-mirror"  # placeholder — your mirror dir
    include = ["registry.terraform.io/fluxvale/fluxvale"]
  }
  direct {}
}
```

A config that declares the provider then plans without any registry
contact:

```hcl
terraform {
  required_providers {
    fluxvale = {
      source = "registry.terraform.io/fluxvale/fluxvale"
    }
  }
}

provider "fluxvale" {
  # api_url = "https://staging.fluxvale.com"  # staging override
  # token: set here only if you must — the attribute wins over the
  # FLUXVALE_PAT environment variable, which is the preferred default.
}
```

Upgrade = download the new version's zips into the mirror and bump the
required version in your config; versions sit side by side.

## Dev loop (dev_overrides)

Working on the provider itself — no mirror, no release needed:

```sh
cd apps/tfprovider && go build -o ~/.tfplugins/terraform-provider-fluxvale .
```

```hcl
provider_installation {
  dev_overrides {
    "registry.terraform.io/fluxvale/fluxvale" = "/home/you/.tfplugins"
  }
  direct {}
}
```

No `init` required (OpenTofu warns that init is unnecessary with dev
overrides); rebuild + re-run. See the README's dev-loop section for
the debugger-attached variant (`go run . -debug`).

## The registry, later

The eventual switch is a dedicated registry repository with GPG-signed
releases — deferred until there are users beyond the owner (the
compatibility contract in [compatibility.md](compatibility.md) is what
makes that switch non-breaking).
