---
page_title: "Secrets in Terraform"
---

# Secrets in Terraform

Terraform has four ways to carry a secret, and they are not equal. This
guide draws the line, then shows the strongest input the provider offers
and the encrypted-state baseline that backs it up.

| Layer | What it protects | What it does not |
| --- | --- | --- |
| `sensitive = true` | Redacts values in console output | The plaintext still lands in state and plan files |
| State and plan encryption | State and plan files at rest, including the backend copy | A value read from a plan by someone with the key |
| `secret_env_vars` (write-only) | Values never enter state or plan at all | The plaintext in your `.tf`/`.tfvars` if you write it there |
| `enc:v1:` marker (ADR-0053, design only) | Ciphertext in state, git, and the platform database — once built | A compromised platform process — the trust boundary |

**The baseline is encryption. The strongest input is write-only. The
marker is the portable fallback design, not shipped yet.** `sensitive`
alone is a display setting, not a security boundary: it only means
Terraform hides the value in human-readable output while writing the
plaintext to the state file.

## The OpenTofu encrypted-state quickstart

OpenTofu encrypts state and plan files client-side. Terraform has no
native equivalent, so a Terraform 1.12+ user gets the write-only input
but must rely on the backend's encryption at rest (S3 SSE, GCS CMEK,
and so on) instead of this block. Keep the passphrase in your keychain
or a secret manager, never in the repository:

```sh
# macOS Keychain
export TF_VAR_state_passphrase="$(security find-generic-password -w -s fluxvale-state 2>/dev/null)"

# Linux secret-tool
export TF_VAR_state_passphrase="$(secret-tool lookup service fluxvale-state)"

# 1Password CLI
export TF_VAR_state_passphrase="$(op read 'op://Private/fluxvale-state/passphrase')"
```

Declare the passphrase as a sensitive variable and enforce encryption
for both files:

```hcl
variable "state_passphrase" {
  type      = string
  sensitive = true
}

terraform {
  required_version = ">= 1.13.0" # OpenTofu: write-only inputs + encryption

  encryption {
    key_provider "pbkdf2" "state" {
      passphrase = var.state_passphrase
    }

    method "aes_gcm" "state" {
      keys = key_provider.pbkdf2.state
    }

    state {
      method   = method.aes_gcm.state
      enforced = true
    }

    plan {
      method   = method.aes_gcm.state
      enforced = true
    }
  }
}
```

`enforced = true` refuses to write an unencrypted state or plan file if
the key configuration is missing — a silent fallback is the failure this
prevents.

If the project already has an unencrypted state, migrate before
enforcing: add `method "unencrypted" "migrate" {}` and a `fallback {
method = method.unencrypted.migrate }` block to `state` and `plan`, run
one `tofu apply`, then remove the fallback and set `enforced = true`.
Back up the state and the passphrase first: losing the passphrase makes
the state unrecoverable.

## Write-only environment variables

`secret_env_vars` is a Terraform write-only map: the provider receives
the values at apply time, but Terraform never writes them to state or
plan files. `secret_env_vars_version` is the rotation signal, because a
write-only value cannot produce a plan diff on its own:

```hcl
variable "database_url" {
  type      = string
  sensitive = true
  ephemeral = true
}

resource "fluxvale_instance" "app" {
  name        = "app"
  app_version_id = "…"
  cpu_cores   = 1
  memory_mb   = 512
  storage_gb  = 10

  env_vars = {
    LOG_LEVEL = "info"
  }

  secret_env_vars         = { DATABASE_URL = var.database_url }
  secret_env_vars_version = 1
}
```

The provider merges `secret_env_vars` into the same in-place environment
PATCH as `env_vars`. The two maps are disjoint: a key present in both is
a plan-time error when both maps are known (the diagnostic names the
key, never a value); if `secret_env_vars` is unknown at plan time the
check runs at apply, and an overlap fails the update.

`secret_env_vars_keys` is the computed, sorted list of key names the
resource manages. It carries no values, so a plan or a review can show
which names are secret-managed:

```hcl
output "managed_secret_keys" {
  value = fluxvale_instance.app.secret_env_vars_keys
}
```

### Rotate a value

Change the value and bump the integer:

```hcl
  secret_env_vars         = { DATABASE_URL = var.database_url }
  secret_env_vars_version = 2
```

The bump is what Terraform can see; the provider re-sends every key in
the map and the platform rolls the running app onto the new
environment. Bump on **any** value or key-set change — adding a key,
removing one, or moving one between `env_vars` and
`secret_env_vars`. A key-set change with an unchanged version is refused
with the key names, because the provider cannot tell it from a stale
map.

If the app's resource quota cannot accommodate a second pod during the
rolling restart, stop it first: set `running = false`, apply the bump,
then set `running = true` and apply again.

### Remove a key

Removal is **drop-key-plus-bump**: remove the key from the map and bump
the version. The provider diffs the computed key list against the prior
state and removes the dropped keys:

```hcl
  secret_env_vars         = { OTHER_KEY = var.other_key }
  secret_env_vars_version = 3
```

Removing the last key can drop both attributes, or keep an empty map and
bump — both converge:

```hcl
  secret_env_vars         = {}
  secret_env_vars_version = 4
```

Do not remove or add a key without bumping the version: the write-only
map change is invisible to Terraform, and the provider refuses a
key-set change it cannot see a bump for.

### Prove the value is not in state

After an apply, with an unencrypted state:

```sh
grep -c 'the-secret-value' terraform.tfstate
# expected: 0
```

With encryption enabled the file is ciphertext, so a grep of the file is
meaningless (it returns 0 either way). Read the decrypted state instead:

```sh
tofu state pull | grep -c 'the-secret-value'
# expected: 0
```

The computed key list and the version integer do appear in state; the
values do not. The API never echoes environment values either.

### Version floor and compatibility

The additions are additive. Existing configurations need no change and
keep working on the provider's existing Terraform floor. Only the new
attributes require **Terraform 1.12+ or OpenTofu 1.13+**, because
write-only attributes are a newer protocol feature, and they need a
provider release that contains #764 (0.4.0 or later; not yet
tagged — this repository does not release itself). On an older CLI, leave
`secret_env_vars`, `secret_env_vars_version`, and
`secret_env_vars_keys` out of the configuration and use `env_vars` plus
state encryption at rest instead.

Import leaves `secret_env_vars_keys` null: the API cannot tell the
provider which existing environment keys were secret. The first apply
that sets `secret_env_vars` sends the full secret set and records the
key list. Pre-existing secret env keys are therefore not tracked and
cannot be removed by the provider until they are set through
`secret_env_vars` at least once.

### What write-only does not protect

The value is still plaintext in your configuration if you write it as a
literal in `.tf` or `.tfvars`. Use a sensitive, ephemeral variable fed by
your CI secret store or a secret manager:

```hcl
variable "database_url" {
  type      = string
  sensitive = true
  ephemeral = true
}
```

`ephemeral = true` keeps the variable value out of a saved plan as well;
`sensitive` alone does not. Applying a saved plan then requires supplying
the variable again (`TF_VAR_database_url=…`), because an ephemeral value
is never stored. `terraform plan` and `terraform apply` can read the
value from the process environment; the state and plan files cannot.

## The `enc:v1:` marker fallback

The platform has an accepted design for a portable marker,
`enc:v1:<kid>:<payload>`
([ADR-0053](https://github.com/fluxvale/fluxvale/blob/main/docs/adr/00053-terraform-secret-env.md)).
The CLI would encrypt a value with the organization's public key and
print a marker; the platform would open it at deploy and never echo the
plaintext. The marker is opaque to Terraform, so it would work on older
Terraform versions and in GitOps flows where a write-only apply is not
available.

**It is not built yet.** The ADR's platform and CLI slices are deferred;
do not rely on `fluxvale secrets encrypt` or paste an `enc:v1:` value
today. When it ships it will be the fallback, not the default: it leaves
ciphertext in state and in HCL, while `secret_env_vars` leaves nothing.

## KMS variants (OpenTofu)

A passphrase is the simplest key provider. A managed key removes the
shared secret from the operator's environment; use a dedicated key per
state file and enable automatic rotation. These key providers are
OpenTofu-only.

AWS KMS:

```hcl
terraform {
  encryption {
    key_provider "aws_kms" "state" {
      kms_key_id = "arn:aws:kms:us-east-1:111122223333:key/…"
      region     = "us-east-1"
      key_spec   = "AES_256"
    }

    method "aes_gcm" "state" {
      keys = key_provider.aws_kms.state
    }

    state {
      method   = method.aes_gcm.state
      enforced = true
    }
    plan {
      method   = method.aes_gcm.state
      enforced = true
    }
  }
}
```

GCP Cloud KMS:

```hcl
    key_provider "gcp_kms" "state" {
      kms_encryption_key = "projects/PROJECT/locations/global/keyRings/RING/cryptoKeys/KEY"
      key_length         = 32
    }
```

Azure Key Vault:

```hcl
    key_provider "azure_vault" "state" {
      vault_uri      = "https://example-keys.vault.azure.net"
      vault_key_name = "fluxvale-state"
      key_length     = 32
    }
```

OpenBao Transit (the `BAO_ADDR`/`BAO_TOKEN` environment variables supply
the endpoint and token):

```hcl
    key_provider "openbao" "state" {
      key_name = "fluxvale-state"
    }
```

The rest of the `method`/`state`/`plan` blocks are identical to the
passphrase example.
