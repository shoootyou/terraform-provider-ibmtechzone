// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/shoootyou-ext/terraform-provider-ibmtechzone/internal/provider"
)

// Registry source address used for local development and CI.
// The real TFC private registry source (app.terraform.io/hashicorp-ddr-platform-{dev,prod}/techzone)
// is wired in the consuming module's required_providers block at distribution time (RFC §8.1).
const providerAddress = "registry.terraform.io/hashicorp-ddr-platform/techzone"

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
