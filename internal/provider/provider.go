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
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure Provider satisfies the provider.Provider interface at compile time.
var _ provider.Provider = &Provider{}

// Provider implements the techzone Terraform provider.
type Provider struct {
	// version is set by GoReleaser via -ldflags at build time; "dev" in local builds.
	version string
}

// providerModel holds the decoded provider configuration block.
type providerModel struct {
	APIKey  types.String `tfsdk:"api_key"`
	APIBase types.String `tfsdk:"api_base"`
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
//
// api_key is required and sensitive; it falls back to TECHZONE_API_KEY env var.
// api_base is optional; it defaults to https://api.techzone.ibm.com.
func (p *Provider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Provider for IBM TechZone reservations. " +
			"Manages techzone_reservation resources, which provision temporary " +
			"AWS cloud accounts from the TechZone pool.",
		Attributes: map[string]schema.Attribute{
			"api_key": schema.StringAttribute{
				MarkdownDescription: "Bearer token for the TechZone API. " +
					"Can be set via the `TECHZONE_API_KEY` environment variable. " +
					"**Sensitive** — never logged or included in diagnostics.",
				Optional:  true,
				Sensitive: true,
			},
			"api_base": schema.StringAttribute{
				MarkdownDescription: "Base URL for the TechZone API. " +
					"Must use https:// unless the host is a loopback address " +
					"(127.0.0.1, localhost, ::1). Defaults to " +
					"`https://api.techzone.ibm.com`.",
				Optional: true,
			},
		},
	}
}

// ValidateConfig performs provider-level config validation.
//
// Rejects a non-https non-loopback api_base with a diagnostic.
// TODO(kou): implement — emit framework diagnostic for invalid base.
func (p *Provider) ValidateConfig(_ context.Context, req provider.ValidateConfigRequest, resp *provider.ValidateConfigResponse) {
	// E2 stub — validation logic added by Kou.
	_ = req
	_ = resp
}

// Configure initialises provider-level state (HTTP client, api_key, api_base).
//
// Reads api_key from config (with TECHZONE_API_KEY env fallback).
// Builds a *techzone.Client and probes GET /api/my/reservations/all:
//   - status != 200           → "token invalid/expired" diagnostic
//   - status == 200, non-JSON → "token invalid/expired" diagnostic
//   - transport error         → connectivity diagnostic (does NOT blame token)
//   - success                 → stores *Client in resp.DataSourceData and resp.ResourceData
//
// TODO(kou): implement.
func (p *Provider) Configure(_ context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	// E2 stub — apply env fallback, build client, probe, store in response.
	_ = os.Getenv("TECHZONE_API_KEY") // referenced so the import stays
	_ = req
	_ = resp
}

// Resources returns the list of managed resources this provider supports.
// E1: empty — techzone_reservation added in E3.
func (p *Provider) Resources(_ context.Context) []func() resource.Resource {
	return nil
}

// DataSources returns the list of data sources this provider supports.
func (p *Provider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewTokenValidationDataSource,
	}
}
