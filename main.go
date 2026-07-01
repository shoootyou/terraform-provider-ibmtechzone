package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/shoootyou-ext/terraform-provider-ibmtechzone/internal/provider"
)

// Registry source address used for local development and CI.
// The public registry source is registry.terraform.io/shoootyou/ibmtechzone;
// consuming modules reference this address in their required_providers block.
const providerAddress = "registry.terraform.io/shoootyou/ibmtechzone"

func main() {
	var debug bool

	flag.BoolVar(&debug, "debug", false, "set to true to run the provider with support for debuggers like delve")
	flag.Parse()

	opts := providerserver.ServeOpts{
		Address: providerAddress,
		Debug:   debug,
	}

	err := providerserver.Serve(context.Background(), provider.New("dev"), opts)
	if err != nil {
		log.Fatal(err.Error())
	}
}
