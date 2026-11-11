// Command terraform-provider-genaiopengateway is the Terraform provider
// for GenAIOpenGateway (docs/terraform.md). It manages organizations,
// teams, keys, Cedar policies and stored configuration objects over the
// gateway's admin API.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/rnikadi/genaiopengateway/terraform/internal/provider"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	debug := flag.Bool("debug", false, "run with support for debuggers such as delve")
	flag.Parse()
	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/rnikadi/genaiopengateway",
		Debug:   *debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
