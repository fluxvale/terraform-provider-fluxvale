package provider

// Version is the provider's release version — the single source of
// truth. scripts/check-tag-version.sh parses it, the release workflow
// asserts the tf-v* tag equals it, and main.go wires it into both the
// client's User-Agent and the -version flag. Bump it when cutting a
// tf-v* release; a tag that disagrees fails the release.
const Version = "0.1.0"
