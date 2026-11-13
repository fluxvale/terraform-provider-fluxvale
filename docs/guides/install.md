---
page_title: "Installing the provider"
---

# Installing the provider

The provider is on the public registry at
[registry.terraform.io/fluxvale/fluxvale](https://registry.terraform.io/providers/fluxvale/fluxvale/latest):
`terraform init` / `tofu init` pulls the signed provider from there.
For offline, air-gapped, or pinned installs, it also installs from this
repo's GitHub Releases into a local filesystem mirror — or, for dev
work, straight off a freshly built binary via `dev_overrides`.

## 1. Download a release

Grab the zip for your platform from the repo's
[Releases](https://github.com/fluxvale/terraform-provider-fluxvale/releases) (tag `v*`)
plus the versioned checksums (`terraform-provider-fluxvale_<version>_SHA256SUMS`,
covering all four platform zips and the manifest) and its GPG signature
(`terraform-provider-fluxvale_<version>_SHA256SUMS.sig` — a binary detached
signature, verifiable against the committed
[`terraform-provider-signing.asc`](https://github.com/fluxvale/terraform-provider-fluxvale/blob/main/terraform-provider-signing.asc)),
and verify YOUR line of the sums — a single-platform download can't
check the whole set:

```sh
zip=terraform-provider-fluxvale_0.3.0_linux_amd64.zip   # your platform's zip
grep "$zip" terraform-provider-fluxvale_0.3.0_SHA256SUMS | sha256sum -c -   # macOS: shasum -a 256 -c -
gpg --fetch-keys https://github.com/fluxvale/terraform-provider-fluxvale/raw/main/terraform-provider-signing.asc
gpg --verify terraform-provider-fluxvale_0.3.0_SHA256SUMS.sig terraform-provider-fluxvale_0.3.0_SHA256SUMS
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
        terraform-provider-fluxvale_0.3.0_linux_amd64.zip
        terraform-provider-fluxvale_0.3.0_linux_arm64.zip
        terraform-provider-fluxvale_0.3.0_darwin_arm64.zip
        terraform-provider-fluxvale_0.3.0_windows_amd64.zip
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
go build -o ~/.tfplugins/terraform-provider-fluxvale .
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

## The registry switch

This repo IS the registry repository — extracted for exactly that
([ADR-0051](https://github.com/fluxvale/fluxvale/blob/main/docs/adr/00051-tfprovider-repo-extraction.md)):
plain `vX.Y.Z` tags, GPG-signed checksums, the registry manifest per
release, and the live listing at
[registry.terraform.io/fluxvale/fluxvale](https://registry.terraform.io/providers/fluxvale/fluxvale/latest).
The mirror recipe above remains the fallback for pinned or offline
installs (the compatibility contract in
[compatibility.md](compatibility.md) keeps the stable names above
non-breaking across upgrades).
