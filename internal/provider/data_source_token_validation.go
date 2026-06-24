package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure tokenValidationDataSource satisfies the datasource.DataSource interface.
var _ datasource.DataSource = &tokenValidationDataSource{}

// tokenValidationDataSource implements the techzone_token_validation data source.
//
// This is a zero-input, plan-time gate that emits { status = "valid" } when the
// provider's configured token is valid. It reuses the probe result stored in
// providerData (no second HTTP call). It is the drop-in replacement for the
// shell-script `data "shell_script" "token_validation"` from VCDLD-1678.
type tokenValidationDataSource struct {
	// pd is the provider-level data injected by Configure.
	pd *providerData
}

// tokenValidationModel holds the computed attributes for this data source.
type tokenValidationModel struct {
	Status types.String `tfsdk:"status"`
}

// NewTokenValidationDataSource is the factory function registered in DataSources().
func NewTokenValidationDataSource() datasource.DataSource {
	return &tokenValidationDataSource{}
}

// Metadata returns the data source type name.
func (d *tokenValidationDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_token_validation"
}

// Schema returns the schema for the techzone_token_validation data source.
// Zero inputs; one computed output: status = "valid".
func (d *tokenValidationDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Validates that the provider's `api_key` is a valid, " +
			"non-expired TechZone API token. Returns `status = \"valid\"` if the " +
			"token is accepted, or fails the plan with an actionable error otherwise. " +
			"Use as a plan-time gate so an expired token fails fast with one clear " +
			"message instead of cascading into cryptic errors during apply.",
		Attributes: map[string]schema.Attribute{
			"status": schema.StringAttribute{
				MarkdownDescription: "Always `\"valid\"` when the data source succeeds.",
				Computed:            true,
			},
		},
	}
}

// Configure receives the provider-configured data from Configure().
// If ProviderData is nil (validate phase) or the wrong type, we return silently.
func (d *tokenValidationDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		// Normal during the validate phase — no provider data yet.
		return
	}

	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		// Unexpected type — do not panic; surface a diagnostic.
		resp.Diagnostics.AddError(
			"Unexpected provider data type",
			"Expected *providerData in ProviderData.",
		)
		return
	}

	d.pd = pd
}

// Read performs the token validation and sets status = "valid".
//
// Token-gate: if the Configure probe recorded a TokenErr, surface it here as
// an AddError so this data source acts as a plan-time gate (audit fix H2).
// No second HTTP call is made — the probe result is already in pd.TokenErr.
func (d *tokenValidationDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	// Nil pd means validate-only phase (no provider data set yet) — return silently.
	if d.pd == nil {
		return
	}

	// If the token probe failed, surface the error here as a plan-time gate.
	if d.pd.TokenErr != nil {
		if d.pd.TokenErrIsConnectivity {
			resp.Diagnostics.AddError(
				"Could not reach TechZone API",
				d.pd.TokenErr.Error(),
			)
		} else {
			resp.Diagnostics.AddError(
				"TECHZONE_API_KEY is invalid or expired",
				d.pd.TokenErr.Error(),
			)
		}
		return
	}

	_ = resp.State.Set(ctx, tokenValidationModel{
		Status: types.StringValue("valid"),
	})
}
