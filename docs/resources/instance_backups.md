---
page_title: "fluxvale_instance_backups"
---

# fluxvale_instance_backups

An instance's backup destination (R2 first).

A routed save converges the cluster server-side (the credential Secret, the restic repo URL, the quota margin) — with one deliberate platform exception: a save riding `enabled: true→false` (the pause) writes the row only, so a coordinates/secret change boarding a paused row heals on the next save or re-enable. Errors surface verbatim: 400s (attribute/guard refusals), 403s (scope/policy), 404s.

## Example Usage

```hcl
resource "fluxvale_instance_backups" "memos" {
  instance_id       = fluxvale_instance.memos.id
  endpoint          = "https://${var.cloudflare_account_id}.r2.cloudflarestorage.com"
  bucket            = var.backup_bucket
  prefix            = "fluxvale/memos/"
  access_key_id     = var.r2_access_key_id
  secret_access_key = var.r2_secret_access_key
  retention_keep    = 14
  enabled           = true
}
```

## Argument Reference

- `instance_id` (Required) — the instance whose backups this routes.
- `endpoint` / `bucket` / `prefix` (Required) — the S3-compatible destination coordinates.
- `access_key_id` (Required) / `secret_access_key` (Optional, write-only, Sensitive) — the credential. `secret_access_key`: set ⇒ applied (or rotated) with the save; unset ⇒ the stored credential is kept. A FIRST create without it lands quota-only (the API's permissive shape) and backups won't run until a later save supplies it. Kept verbatim in state — the API never echoes it.
- `retention_keep` (Optional) — how many snapshots to keep.
- `enabled` (Optional, bool) — the pause switch.

## Attributes Read

`id` (the settings-row id; reads and imports go by `instance_id`, and the row id refreshes if the row is ever re-created out-of-band).

## Import

`terraform import fluxvale_instance_backups.x <instance_id>`. `secret_access_key` stays null after import; your config's value (or its absence) is the rotation decision on the next apply.
