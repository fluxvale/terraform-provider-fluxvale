package provider

// Version is the provider's release version — the single source of
// truth. scripts/check-tag-version.sh parses it, the release workflow
// asserts the v* tag equals it, and main.go wires it into both the
// client's User-Agent and the -version flag. Bump it when cutting a
// release; a tag that disagrees fails the release. The bump carries
// its doc sweep: docs/guides/install.md and the platform's public doc
// (docs/public/automation/terraform.md in the fluxvale/fluxvale
// monorepo) name the concrete archives.
const Version = "0.3.0"
