package provider

// Version is the provider's release version — the single source of
// truth. scripts/check-tag-version.sh parses it, the release workflow
// asserts the tf-v* tag equals it, and main.go wires it into both the
// client's User-Agent and the -version flag. Bump it when cutting a
// tf-v* release; a tag that disagrees fails the release. The bump
// carries its doc sweep: apps/tfprovider/docs/install.md and
// docs/public/automation/terraform.md name the concrete archives.
const Version = "0.1.1"
