package provider

// Token-safety contract (RFC §4, Ei F-02): the api_key is accessed only via
// the *techzone.Client.  No token value appears in diagnostics or error strings.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure reservationDataSource satisfies the datasource.DataSource interface.
var _ datasource.DataSource = &reservationDataSource{}

// reservationDataSource implements the ibmtechzone_reservation data source
// (read-only lookup by ID — no prune, no lifecycle management).
type reservationDataSource struct {
	pd *providerData
}

// reservationDataSourceModel is the Terraform state model for the data source.
type reservationDataSourceModel struct {
	// Required input.
	ID types.String `tfsdk:"id"`

	// Computed outputs.
	Status       types.String `tfsdk:"status"`
	ServiceLinks types.List   `tfsdk:"service_links"`
	StartDate    types.String `tfsdk:"start_date"`
	EndDate      types.String `tfsdk:"end_date"`
}

// NewReservationDataSource is the factory function registered in DataSources().
func NewReservationDataSource() datasource.DataSource {
	return &reservationDataSource{}
}

func (d *reservationDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_reservation"
}

func (d *reservationDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads an existing IBM TechZone reservation by ID. " +
			"Read-only — does not manage the reservation lifecycle. " +
			"Useful for read-only references and debugging without creating a managed resource.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "TechZone reservation ID to look up.",
				Required:            true,
			},
			"status": schema.StringAttribute{
				MarkdownDescription: "Current reservation status.",
				Computed:            true,
			},
			"service_links": schema.ListNestedAttribute{
				MarkdownDescription: "Service links attached to this reservation.",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"type": schema.StringAttribute{
							MarkdownDescription: "Service link type.",
							Computed:            true,
						},
						"url": schema.StringAttribute{
							MarkdownDescription: "Service link URL.",
							Computed:            true,
						},
					},
				},
			},
			"start_date": schema.StringAttribute{
				MarkdownDescription: "Reservation start date (`provisionDate` or fallback chain).",
				Computed:            true,
			},
			"end_date": schema.StringAttribute{
				MarkdownDescription: "Reservation end date (`provisionUntil` or fallback chain).",
				Computed:            true,
			},
		},
	}
}

func (d *reservationDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data type",
			"Expected *providerData in ProviderData.",
		)
		return
	}
	d.pd = pd
}

func (d *reservationDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.pd == nil || d.pd.Client == nil {
		resp.Diagnostics.AddError("Provider not configured",
			"The provider has no api_key configured.")
		return
	}
	// Token gate: data source Read is load-bearing (audit fix Sho-B #2).
	if d.pd.TokenErr != nil {
		resp.Diagnostics.AddError("TECHZONE_API_KEY is invalid or expired", d.pd.TokenErr.Error())
		return
	}

	var config reservationDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	reservationID := config.ID.ValueString()
	if reservationID == "" {
		resp.Diagnostics.AddError("Invalid id", "The reservation id must not be empty.")
		return
	}

	httpStatus, body, err := d.pd.Client.DoGet(ctx, "/api/reservation/aws/"+url.PathEscape(reservationID))
	if err != nil {
		resp.Diagnostics.AddError("TechZone API unreachable",
			fmt.Sprintf("GET /api/reservation/aws/%s: %s", reservationID, err.Error()))
		return
	}

	if httpStatus == 404 {
		resp.Diagnostics.AddError("Reservation not found",
			fmt.Sprintf("GET /api/reservation/aws/%s returned 404.", reservationID))
		return
	}

	if httpStatus < 200 || httpStatus >= 300 {
		// Suppress body content at 401/403 — auth-rejection bodies may contain
		// SSO redirect HTML or session metadata (same guard as the poll loop in
		// resource_reservation.go).
		var dsErrDetail string
		if httpStatus == 401 || httpStatus == 403 {
			dsErrDetail = fmt.Sprintf("GET /api/reservation/aws/%s returned HTTP %d.",
				reservationID, httpStatus)
		} else {
			dsErrDetail = fmt.Sprintf("GET /api/reservation/aws/%s returned HTTP %d. Body: %s",
				reservationID, httpStatus, truncate(body, 512))
		}
		resp.Diagnostics.AddError("TechZone API error", dsErrDetail)
		return
	}

	var apiResp tzReservationResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		resp.Diagnostics.AddError("Failed to parse TechZone response",
			fmt.Sprintf("GET /api/reservation/aws/%s: %s", reservationID, err.Error()))
		return
	}

	// Map to data source model (no prune — caller decides what to do with the status).
	state := reservationDataSourceModel{
		ID:     types.StringValue(reservationID),
		Status: types.StringValue(derefString(apiResp.Status)),
		StartDate: types.StringValue(firstNonEmpty(
			derefString(apiResp.ProvisionDate),
			derefString(apiResp.Start),
			derefString(apiResp.StartDate),
		)),
		EndDate: types.StringValue(firstNonEmpty(
			derefString(apiResp.ProvisionUntil),
			derefString(apiResp.End),
			derefString(apiResp.EndDate),
		)),
	}

	links := make([]ServiceLinkModel, 0, len(apiResp.ServiceLinks))
	for _, sl := range apiResp.ServiceLinks {
		links = append(links, ServiceLinkModel{
			Type: types.StringValue(sl.Type),
			URL:  types.StringValue(sl.URL),
		})
	}
	listVal, diags := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: serviceLinkAttrTypes}, links)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.ServiceLinks = listVal

	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}
