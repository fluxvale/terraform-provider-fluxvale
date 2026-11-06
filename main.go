// The FluxVale Terraform/OpenTofu provider entrypoint — the
// terraform-plugin-framework serve pattern. Run with -debug to
// register for terraform's dev_overrides (managed debug).
package main

import (
	"context"
	"flag"
	"log"

	"github.com/fluxvale/fluxvale/apps/tfprovider/internal/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
)

// version rides the client's User-Agent; wire the real value at
// release build time via -ldflags "-X main.version=vX.Y.Z".
var version = "dev"

func main() {
	var debug bool

	flag.BoolVar(&debug, "debug", false, "run with support for terraform's dev_overrides")
	flag.Parse()

	opts := providerserver.ServeOpts{
		Address: "registry.terraform.io/fluxvale/fluxvale",
		Debug:   debug,
	}

	err := providerserver.Serve(context.Background(), provider.New(version), opts)

	if err != nil {
		log.Fatal(err.Error())
	}
}
