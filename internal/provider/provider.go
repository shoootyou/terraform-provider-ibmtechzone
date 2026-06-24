// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package provider implements the terraform-provider-ibmtechzone plugin.
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
	"encoding/json"
	"fmt"
	"net/url"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/shoootyou-ext/terraform-provider-ibmtechzone/internal/techzone"
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

// providerData is passed to every resource and data source via
// resp.ResourceData / resp.DataSourceData. It carries both the client and any
// token-probe error that occurred during Configure.
//
// Separation rationale (RFC §6 / audit fix H2):
//   - Create and Read are load-bearing: they AddError if TokenErr != nil.
//   - Delete MUST succeed even when the token is expired (RFC §3.3 / §4 M1);
//     it ignores TokenErr entirely.
//   - The techzone_token_validation data source Read surfaces TokenErr as an
//     AddError so it can act as a plan-time gate.
//   - TokenErrIsConnectivity distinguishes a network/transport failure from a
//     token-invalid failure so callers can emit the right diagnostic summary.
type providerData struct {
	Client                 *techzone.Client
	TokenErr               error
	TokenErrIsConnectivity bool
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

// isLoopbackHost reports whether host is a loopback address (without port).
func isLoopbackHost(host string) bool {
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// ValidateConfig performs provider-level config validation.
//
// Rejects a non-https non-loopback api_base with a diagnostic.
func (p *Provider) ValidateConfig(ctx context.Context, req provider.ValidateConfigRequest, resp *provider.ValidateConfigResponse) {
	var config providerModel
	diags := req.Config.Get(ctx, &config)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// If api_base is not set or unknown, nothing to validate.
	if config.APIBase.IsNull() || config.APIBase.IsUnknown() {
		return
	}

	apiBase := config.APIBase.ValueString()
	parsed, err := url.Parse(apiBase)
	if err != nil {
		resp.Diagnostics.AddAttributeError(
			path.Root("api_base"),
			"Invalid api_base URL",
			"api_base must be a valid URL. "+
				"Got: "+apiBase,
		)
		return
	}

	if parsed.Scheme != "https" && !isLoopbackHost(parsed.Hostname()) {
		resp.Diagnostics.AddAttributeError(
			path.Root("api_base"),
			"api_base must use https://",
			"The api_base URL must use https:// unless the host is a loopback "+
				"address (127.0.0.1, localhost, ::1). "+
				"Got: "+apiBase,
		)
	}
}

// Configure initialises provider-level state (HTTP client, api_key, api_base).
//
// Token-gate refactor (audit fix H2 / RFC §6):
// Configure always stores a *providerData in resp.ResourceData/DataSourceData.
// The token probe result is stored in providerData.TokenErr — Configure NEVER
// calls AddError for a probe failure. This allows terraform destroy to proceed
// even when the token is expired: Delete reads providerData but ignores TokenErr.
//
// Probe decision order:
//  1. Transport error → connectivity problem (does NOT blame token).
//  2. status != 200 → token expired/invalid.
//  3. 200 + !json.Valid(body) → token invalid (SSO HTML redirect page).
//  4. 200 + json.Valid(body) → token valid.
func (p *Provider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config providerModel
	diags := req.Config.Get(ctx, &config)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Resolve api_base — default to production endpoint, strip any trailing slash.
	apiBase := "https://api.techzone.ibm.com"
	if !config.APIBase.IsNull() && !config.APIBase.IsUnknown() {
		apiBase = config.APIBase.ValueString()
	}

	// Resolve api_key — attr first, then env fallback.
	// Read exactly once into a local variable; never reference config.APIKey again.
	var apiKey string
	if config.APIKey.IsNull() || config.APIKey.IsUnknown() {
		apiKey = os.Getenv("TECHZONE_API_KEY")
	} else {
		apiKey = config.APIKey.ValueString()
	}

	// If no api_key is available, store an empty providerData so resources can
	// emit a clear "no key" error when they need one.
	if apiKey == "" {
		pd := &providerData{}
		resp.DataSourceData = pd
		resp.ResourceData = pd
		return
	}

	// Build the client (scheme/loopback validation also happens here).
	client, err := techzone.NewClient(apiBase, apiKey)
	if err != nil {
		resp.Diagnostics.AddError(
			"Invalid provider configuration",
			"Could not create TechZone client: "+err.Error(),
		)
		return
	}

	// Probe the reservations endpoint to validate the token.
	// Result is stored in providerData.TokenErr — never AddError here.
	const probePath = "/api/my/reservations/all"
	var tokenErr error
	var tokenErrIsConnectivity bool

	status, body, probeErr := client.DoGet(ctx, probePath)
	if probeErr != nil {
		// Transport failure — do NOT blame the token. Store a connectivity error
		// so Create/Read can surface it, but allow Delete to proceed.
		tokenErr = fmt.Errorf(
			"a network error prevented connecting to the TechZone API. "+
				"Check your network connectivity and the api_base setting. "+
				"Transport error: %s", probeErr.Error(),
		)
		tokenErrIsConnectivity = true
	} else if status != 200 {
		// Non-200 (including 3xx that were not followed) → token is invalid/expired.
		tokenErr = fmt.Errorf(
			"TECHZONE_API_KEY is invalid or expired. " +
				"Refresh it at https://techzone.ibm.com and re-run.",
		)
	} else if !json.Valid(body) {
		// 200 but non-JSON body (e.g. SSO HTML page) → token is invalid/expired.
		tokenErr = fmt.Errorf(
			"TECHZONE_API_KEY is invalid or expired. " +
				"Refresh it at https://techzone.ibm.com and re-run.",
		)
	}

	// Always store client + probe result. Resources decide what to do with TokenErr.
	pd := &providerData{
		Client:                 client,
		TokenErr:               tokenErr,
		TokenErrIsConnectivity: tokenErrIsConnectivity,
	}
	resp.DataSourceData = pd
	resp.ResourceData = pd
}

// Resources returns the list of managed resources this provider supports.
func (p *Provider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewReservationResource,
	}
}

// DataSources returns the list of data sources this provider supports.
func (p *Provider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewTokenValidationDataSource,
		NewReservationDataSource,
	}
}
