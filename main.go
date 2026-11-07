// The FluxVale Terraform/OpenTofu provider entrypoint — the
// terraform-plugin-framework serve pattern. Run with -debug to
// register for terraform's dev_overrides (managed debug).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/fluxvale/fluxvale/apps/tfprovider/internal/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
)

func main() {
	var debug, showVersion bool

	flag.BoolVar(&debug, "debug", false, "run with support for terraform's dev_overrides")
	// The release build's version proof: the release workflow runs
	// `-version` per leg and asserts it carries the tag's version. One
	// source — provider.Version — feeds this and the User-Agent.
	flag.BoolVar(&showVersion, "version", false, "print the provider version and exit")
	flag.Parse()

	if showVersion {
		fmt.Println("terraform-provider-fluxvale " + provider.Version)
		return
	}

	opts := providerserver.ServeOpts{
		Address: "registry.terraform.io/fluxvale/fluxvale",
		Debug:   debug,
	}

	err := providerserver.Serve(context.Background(), provider.New(provider.Version), opts)

	if err != nil {
		log.Fatal(err.Error())
	}
}
