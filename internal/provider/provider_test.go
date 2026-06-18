// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider_test

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/shoootyou-ext/terraform-provider-techzone/internal/provider"
)

// testAccProtoV6ProviderFactories is shared by acceptance tests in this package.
// It wires the local provider implementation into the testing framework's
// provider factory without requiring a running registry.
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"techzone": providerserver.NewProtocol6WithError(provider.New("test")()),
}

// TestProvider_schema verifies that:
//   - Provider satisfies the provider.Provider interface (compile-time assertion in provider.go).
//   - The provider schema is valid and the framework can instantiate it without errors.
//
// This is intentionally minimal for E1: it confirms the scaffold compiles, the
// providerserver wires up cleanly, and no diagnostic errors occur on an empty config.
// Richer Configure/schema tests land in E2 once Shin writes the failing tests for
// api_key and api_base.
func TestProvider_schema(t *testing.T) {
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// An empty provider block is valid for E1 (no required attributes yet).
				Config: `provider "techzone" {}`,
			},
		},
	})
}
