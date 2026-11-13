---
page_title: "fluxvale_pocket_id_sso"
---

# fluxvale_pocket_id_sso

An instance's PocketID sign-in wiring — the typed record that the instance's OIDC environment is backed by an OIDC client the platform minted on your PocketID instance and continues to hold. The client secret's **value** never travels through Terraform: the platform writes it into the instance's environment and keeps only identifiers on the wire.

## Example Usage

```hcl
resource "fluxvale_pocket_id_sso" "vault" {
  instance_id           = fluxvale_instance.vaultwarden.id
  pocket_id_instance_id = fluxvale_instance.pocket_id.id
  rotate_secret_on      = "2026-11-13" # bump to rotate the client secret in place
}
```

## Argument Reference

- `instance_id` (Required) — the app instance whose sign-in is wired (`fluxvale_instance.id`). **One wiring per instance**, so this is also the resource's `id`. Changing it replaces the resource.
- `pocket_id_instance_id` (Required) — the PocketID instance that serves the sign-in (`fluxvale_instance.id` in the same organization). The platform requires it to be public and to have held-key custody (the operator stores its API key once); a refusal — wrong org, private, no custody — surfaces verbatim. Changing it replaces the resource.
- `rotate_secret_on` (Optional) — the rotation trigger: an arbitrary value (a date, a version, a nonce). Creating the resource connects; **changing this value rotates the client secret in place** — a second active secret is minted, the new value is applied to the workload, then the old secret is deleted once the workload patch is accepted. The delete is best effort: a refused PocketID delete leaves the old secret active there, so check and remove it by hand. Pods still on the old value return 401 until the rollout cycles them. The client id and the resource id never change: rotation is an update, **not** a replacement (a replacement would disconnect and mint a brand-new client, losing the client id). The platform's refusals (a not-yet-deployed wiring, a lost/deleting instance) surface verbatim. Null means no rotation trigger; the value is config-only posture and has no wire echo.

## Attributes Read

- `id` — the **instance id** (the one-wiring key). Stable across rotations and refreshes; it is deliberately not the wiring row's id, which changes on a re-wire.
- `pocket_id_client_id` — the OIDC client id PocketID minted. An identifier, not a credential; stable across secret rotations, a replacement mints a new one.
- `pocket_id_secret_id` — PocketID's id for the client's live secret. An identifier, never the secret's value; rotation moves this id.
- `wired_at` — when the client was minted and the env merged (the wiring row's create timestamp; a re-wire writes a fresh one).

## Notes

- **Feature flag.** The platform gates connect, disconnect, and rotate behind the `pocketid_sso_wiring` feature flag (seeded enabled). The read stays available while the flag is off — a refresh must not lose state during a shutdown window. If a mutation answers 400 with the flag's name, ask the platform operator to enable it.
- **Connect dispatch.** The platform decides the path: a not-yet-deployed instance (pending/error/lost) ships the SSO environment on its next deploy; a deployed instance is retrofitted in place (row + running workload). A mid-rollout (`deploying`/`restoring`) instance answers 400 busy, surfaced verbatim.
- **Delete is idempotent.** Destroy calls disconnect: a live wiring deletes the PocketID client (best effort), strips the SSO environment from the row and the running workload, then clears the wiring. No wiring is a success (`client_deleted: false`), and a 404 is already-gone — both converge for Terraform.
- **Replacements.** `instance_id` and `pocket_id_instance_id` force replacement; the identity pair names a different wiring. Only `rotate_secret_on` changes in place.
- **Secrets.** Neither the PocketID secret value nor the instance's environment is ever echoed by the API. This resource manages the relationship and its identifiers, never the credential value.
