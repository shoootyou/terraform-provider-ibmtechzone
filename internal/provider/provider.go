// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package provider implements the terraform-provider-techzone plugin.
//
// # Token-safety contract (RFC §4, Ei F-02)
//
// The api_key value is read from provider config exactly once and passed only as
// an Authorization header value. It is never formatted into logs, diagnostics, or
// error strings. No request/response dumping (httputil.DumpRequest et al.) is
// permitted at any log level. Error strings from HTTP calls include only: HTTP
// status code, URL without query params, and a response-body excerpt — never
// request headers or the token value. These constraints apply to Configure and
// every Create/Read/Delete call site.
package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// Ensure Provider satisfies the provider.Provider interface at compile time.
var _ provider.Provider = &Provider{}

// Provider implements the techzone Terraform provider.
//
// In E1 this is an empty scaffold. Provider configuration (api_key, api_base),
// resources (techzone_reservation), and data sources (techzone_token_validation,
// techzone_reservation) are added in E2–E4 after Shin writes their failing tests.
type Provider struct {
	// version is set by GoReleaser via -ldflags at build time; "dev" in local builds.
	version string
}

// New returns a provider factory function. Called by main.go and by the acceptance
// test helper. The version string is injected at build time by GoReleaser.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &Provider{version: version}
	}
}

// Metadata sets the provider type name and version.
func (p *Provider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "techzone"
	resp.Version = p.version
}

// Schema returns the provider configuration schema.
// E1: empty — api_key and api_base are added in E2 after Shin writes the tests.
func (p *Provider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Provider for IBM TechZone reservations. " +
			"Manages techzone_reservation resources, which provision temporary " +
			"AWS cloud accounts from the TechZone pool.",
	}
}

// Configure initialises provider-level state (HTTP client, api_key, api_base).
// E1: no-op — configuration logic added in E2.
func (p *Provider) Configure(_ context.Context, _ provider.ConfigureRequest, _ *provider.ConfigureResponse) {
}

// Resources returns the list of managed resources this provider supports.
// E1: empty — techzone_reservation added in E3.
func (p *Provider) Resources(_ context.Context) []func() resource.Resource {
	return nil
}

// DataSources returns the list of data sources this provider supports.
// E1: empty — techzone_token_validation and techzone_reservation added in E4.
func (p *Provider) DataSources(_ context.Context) []func() datasource.DataSource {
	return nil
}
