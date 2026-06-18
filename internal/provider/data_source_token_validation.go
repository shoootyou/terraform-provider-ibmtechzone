// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

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
// provider's configured token is valid. It reuses the *techzone.Client stored in
// provider Configure (no second HTTP call). It is the drop-in replacement for the
// shell-script `data "shell_script" "token_validation"` from VCDLD-1678.
type tokenValidationDataSource struct {
	// client is the provider-level *techzone.Client injected by Configure.
	// TODO(kou): type as *techzone.Client once the package is wired in.
	client interface{}
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

// Configure receives the provider-configured client from Configure().
// TODO(kou): implement — assert client type, store in d.client.
func (d *tokenValidationDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	// E2 stub.
	if req.ProviderData == nil {
		return
	}
	d.client = req.ProviderData
}

// Read performs the token validation and sets status = "valid".
// TODO(kou): implement — invoke client probe, set status, emit diagnostics on failure.
func (d *tokenValidationDataSource) Read(_ context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	// E2 stub — always sets status to empty, causing the acceptance test to fail (RED).
	_ = resp.State.Set(context.Background(), tokenValidationModel{
		Status: types.StringValue(""),
	})
}
