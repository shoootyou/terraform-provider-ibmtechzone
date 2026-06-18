// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package provider

// Token-safety contract (RFC §4, Ei F-02): the api_key is accessed only via
// the *techzone.Client, which passes it exclusively as an Authorization header
// value.  It is never formatted into logs, diagnostics, or error strings here.
// No httputil.DumpRequest/DumpResponse anywhere in this file.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/shoootyou-ext/terraform-provider-techzone/internal/techzone"
)

// Ensure reservationResource satisfies the resource.Resource interface.
var _ resource.Resource = &reservationResource{}
var _ resource.ResourceWithImportState = &reservationResource{}

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// reservationResource implements the techzone_reservation managed resource.
type reservationResource struct {
	client *techzone.Client
}

// reservationModel is the Terraform state model for techzone_reservation.
type reservationModel struct {
	// Identity inputs (RequiresReplace)
	Template             types.String `tfsdk:"template"`
	Region               types.String `tfsdk:"region"`
	ReservationName      types.String `tfsdk:"reservation_name"`
	Purpose              types.String `tfsdk:"purpose"`
	CollectionID         types.String `tfsdk:"collection_id"`
	UserEmail            types.String `tfsdk:"user_email"`
	HCPOrg               types.String `tfsdk:"hcp_org"`
	HCPProject           types.String `tfsdk:"hcp_project"`

	// Operational inputs (no RequiresReplace)
	ReservationDurationDays types.Int64 `tfsdk:"reservation_duration_days"`
	TimeoutMinutes          types.Int64 `tfsdk:"timeout_minutes"`

	// Computed outputs
	ID           types.String `tfsdk:"id"`
	Status       types.String `tfsdk:"status"`
	ServiceLinks types.List   `tfsdk:"service_links"`
	StartDate    types.String `tfsdk:"start_date"`
	EndDate      types.String `tfsdk:"end_date"`
}

// ServiceLinkModel is the element type for service_links.
type ServiceLinkModel struct {
	Type types.String `tfsdk:"type"`
	URL  types.String `tfsdk:"url"`
}

// serviceLinkAttrTypes maps the service_links element object attribute types.
var serviceLinkAttrTypes = map[string]attr.Type{
	"type": types.StringType,
	"url":  types.StringType,
}

// ---------------------------------------------------------------------------
// Wire-decode types (decoded from raw TechZone JSON, NOT Framework types)
// ---------------------------------------------------------------------------

// tzReservationResponse is the shape returned by GET /api/reservation/aws/<id>.
// Fields that may be JSON null are decoded as *string so that JSON null → nil
// (treated as ""), and the literal string "null" (from jq -r) → non-nil "*string"
// holding "null" — both cases handled identically (as "") but kept distinct
// internally so PastExpiry can receive the correct value.
type tzReservationResponse struct {
	ID            string       `json:"id"`
	Status        *string      `json:"status"`
	ServiceLinks  []tzSvcLink  `json:"serviceLinks"`
	ProvisionDate *string      `json:"provisionDate"`
	Start         *string      `json:"start"`
	StartDate     *string      `json:"startDate"`
	ProvisionUntil *string     `json:"provisionUntil"`
	End           *string      `json:"end"`
	EndDate       *string      `json:"endDate"`
}

// tzSvcLink is one element from the serviceLinks array.
type tzSvcLink struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// tzCreateResponse is the minimal shape returned by POST /api/reservation/aws.
type tzCreateResponse struct {
	ID string `json:"id"`
}

// tzPollResponse is the minimal shape used during the poll loop.
type tzPollResponse struct {
	Status *string `json:"status"`
}

// ---------------------------------------------------------------------------
// Factory + metadata
// ---------------------------------------------------------------------------

// NewReservationResource is the factory function registered in Resources().
func NewReservationResource() resource.Resource {
	return &reservationResource{}
}

func (r *reservationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_reservation"
}

// ---------------------------------------------------------------------------
// Schema
// ---------------------------------------------------------------------------

func (r *reservationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an IBM TechZone AWS account reservation. " +
			"Models the lifecycle of a temporary AWS account provisioned from the " +
			"TechZone pool.\n\n" +
			"> **Note:** Identity attributes (`template`, `region`, `reservation_name`, " +
			"`purpose`, `collection_id`, `user_email`, `hcp_org`, `hcp_project`) trigger " +
			"replacement when changed. `timeout_minutes` and `reservation_duration_days` " +
			"are operational and do not trigger replacement.",
		Attributes: map[string]schema.Attribute{
			// --- Identity inputs (RequiresReplace) ---
			"template": schema.StringAttribute{
				MarkdownDescription: "TechZone template name. Defaults to `aws-account-hashicorp-ddr`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("aws-account-hashicorp-ddr"),
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"region": schema.StringAttribute{
				MarkdownDescription: "AWS region for the reservation. Defaults to `us-east-2`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("us-east-2"),
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"reservation_name": schema.StringAttribute{
				MarkdownDescription: "Name for the reservation. Defaults to `Hashicorp DDR`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("Hashicorp DDR"),
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"purpose": schema.StringAttribute{
				MarkdownDescription: "Reservation purpose. Defaults to `Demo`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("Demo"),
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"collection_id": schema.StringAttribute{
				MarkdownDescription: "TechZone collection ID for this reservation. Required.",
				Required:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"user_email": schema.StringAttribute{
				MarkdownDescription: "IBM ID (email) of the reservation owner. Required. " +
					"Also used as the `IBMID` in the delete payload.",
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"hcp_org": schema.StringAttribute{
				MarkdownDescription: "HCP organization ID injected as `_04_hcp_org` dynamic output. Required.",
				Required:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"hcp_project": schema.StringAttribute{
				MarkdownDescription: "HCP project ID injected as `_05_hcp_project` dynamic output. Required.",
				Required:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},

			// --- Operational inputs (no RequiresReplace) ---
			"reservation_duration_days": schema.Int64Attribute{
				MarkdownDescription: "Reservation duration in days. Applies at create time only. " +
					"Changing this value does NOT modify the existing reservation; " +
					"the new duration applies on the next recreate.",
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(1),
			},
			"timeout_minutes": schema.Int64Attribute{
				MarkdownDescription: "Maximum minutes to wait for the reservation to reach `Ready` status. " +
					"Defaults to `30`. Changing this does not recreate the reservation.",
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(30),
			},

			// --- Computed outputs ---
			"id": schema.StringAttribute{
				MarkdownDescription: "TechZone reservation ID.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"status": schema.StringAttribute{
				MarkdownDescription: "Current reservation status (e.g. `Ready`, `Provisioning`).",
				Computed:            true,
			},
			"service_links": schema.ListNestedAttribute{
				MarkdownDescription: "Service links attached to this reservation.",
				Computed:            true,
				PlanModifiers:       []planmodifier.List{listplanmodifier.UseStateForUnknown()},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"type": schema.StringAttribute{
							MarkdownDescription: "Service link type (e.g. `AWS Console`).",
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
				MarkdownDescription: "Reservation start date from `provisionDate` (or fallback chain).",
				Computed:            true,
			},
			"end_date": schema.StringAttribute{
				MarkdownDescription: "Reservation end date from `provisionUntil` (or fallback chain).",
				Computed:            true,
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Configure — receive client from provider
// ---------------------------------------------------------------------------

func (r *reservationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*techzone.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data type",
			"Expected *techzone.Client in ProviderData.",
		)
		return
	}
	r.client = client
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

func (r *reservationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if r.client == nil {
		resp.Diagnostics.AddError("Provider not configured",
			"The provider has no api_key configured. Configure the provider with a valid TechZone API key.")
		return
	}

	var plan reservationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Compute start/end timestamps: start = now+1min, end = now+1min+duration_days.
	now := time.Now().UTC()
	start := now.Add(1 * time.Minute).Format("2006-01-02T15:04:05.000Z")
	durationDays := plan.ReservationDurationDays.ValueInt64()
	end := now.Add(1*time.Minute + time.Duration(durationDays)*24*time.Hour).Format("2006-01-02T15:04:05.000Z")

	input := techzone.CreateInput{
		Name:         plan.ReservationName.ValueString(),
		Purpose:      plan.Purpose.ValueString(),
		User:         plan.UserEmail.ValueString(),
		Region:       plan.Region.ValueString(),
		CollectionID: plan.CollectionID.ValueString(),
		HCPOrg:       plan.HCPOrg.ValueString(),
		HCPProject:   plan.HCPProject.ValueString(),
		Start:        start,
		End:          end,
	}

	payload, err := techzone.BuildCreatePayload(input)
	if err != nil {
		resp.Diagnostics.AddError("Failed to build create payload", err.Error())
		return
	}

	// POST /api/reservation/aws
	status, body, err := r.client.DoPost(ctx, "/api/reservation/aws", payload)
	if err != nil {
		resp.Diagnostics.AddError("TechZone API unreachable", fmt.Sprintf("POST /api/reservation/aws: %s", err.Error()))
		return
	}
	if status < 200 || status >= 300 {
		resp.Diagnostics.AddError(
			"TechZone reservation create failed",
			fmt.Sprintf("POST /api/reservation/aws returned HTTP %d. Body: %s", status, truncate(body, 512)),
		)
		return
	}

	var createResp tzCreateResponse
	if err := json.Unmarshal(body, &createResp); err != nil || createResp.ID == "" {
		resp.Diagnostics.AddError(
			"TechZone reservation create: unexpected response",
			fmt.Sprintf("Could not parse reservation ID from response. Body: %s", truncate(body, 512)),
		)
		return
	}

	reservationID := createResp.ID

	// Poll loop: GET /api/reservation/<id> every 10s until Ready or terminal.
	timeoutMinutes := plan.TimeoutMinutes.ValueInt64()
	maxAttempts := timeoutMinutes * 6 // 6 × 10s = 60s per minute
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for attempt := int64(1); attempt <= maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			resp.Diagnostics.AddError("Context cancelled", "Terraform interrupted while waiting for reservation to become Ready.")
			return
		case <-ticker.C:
		}

		pollStatus, pollBody, pollErr := r.client.DoGet(ctx, "/api/reservation/"+reservationID)
		if pollErr != nil {
			// Transient connectivity error — log and retry.
			continue
		}
		if pollStatus < 200 || pollStatus >= 300 {
			// Transient non-2xx — retry (bounded).
			continue
		}

		var pollResp tzPollResponse
		if err := json.Unmarshal(pollBody, &pollResp); err != nil || pollResp.Status == nil {
			// Non-JSON or status-less body — retry.
			continue
		}

		s := *pollResp.Status
		switch s {
		case "Failed":
			resp.Diagnostics.AddError(
				"TechZone reservation failed",
				fmt.Sprintf("Reservation %s transitioned to Failed. Body: %s", reservationID, truncate(pollBody, 512)),
			)
			return
		case "Deleted", "Expired":
			resp.Diagnostics.AddError(
				"TechZone reservation terminated unexpectedly",
				fmt.Sprintf("Reservation %s reached terminal status %q during provisioning.", reservationID, s),
			)
			return
		case "Ready":
			// Break out of the poll loop.
			goto pollDone
		}
	}

	resp.Diagnostics.AddError(
		"Timeout waiting for reservation",
		fmt.Sprintf("Reservation %s did not reach Ready within %d minutes.", reservationID, timeoutMinutes),
	)
	return

pollDone:
	// Final canonical GET: /api/reservation/aws/<id> — bounded retry for the
	// documented improvement (RFC §3.1: the bash final GET was un-hardened).
	var finalResp *tzReservationResponse
	for attempt := int64(0); attempt < maxAttempts; attempt++ {
		canStatus, canBody, canErr := r.client.DoGet(ctx, "/api/reservation/aws/"+reservationID)
		if canErr != nil {
			select {
			case <-ctx.Done():
				resp.Diagnostics.AddError("Context cancelled", "Interrupted during final read after reservation became Ready.")
				return
			case <-ticker.C:
			}
			continue
		}
		if canStatus < 200 || canStatus >= 300 {
			select {
			case <-ctx.Done():
				resp.Diagnostics.AddError("Context cancelled", "Interrupted during final read after reservation became Ready.")
				return
			case <-ticker.C:
			}
			continue
		}
		var r2 tzReservationResponse
		if err := json.Unmarshal(canBody, &r2); err != nil {
			select {
			case <-ctx.Done():
				resp.Diagnostics.AddError("Context cancelled", "Interrupted during final read after reservation became Ready.")
				return
			case <-ticker.C:
			}
			continue
		}
		finalResp = &r2
		break
	}
	if finalResp == nil {
		resp.Diagnostics.AddError(
			"Failed to read reservation after creation",
			fmt.Sprintf("GET /api/reservation/aws/%s did not return a parseable response within the timeout.", reservationID),
		)
		return
	}

	// Map API response → state.
	state := mapResponseToModel(plan, finalResp)
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

// ---------------------------------------------------------------------------
// Read
// ---------------------------------------------------------------------------

func (r *reservationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if r.client == nil {
		return
	}

	var state reservationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	reservationID := state.ID.ValueString()
	if reservationID == "" {
		// No ID in state — nothing to read.
		resp.State.RemoveResource(ctx)
		return
	}

	httpStatus, body, err := r.client.DoGet(ctx, "/api/reservation/aws/"+reservationID)
	if err != nil {
		resp.Diagnostics.AddError("TechZone API unreachable during Read",
			fmt.Sprintf("GET /api/reservation/aws/%s: %s", reservationID, err.Error()))
		return
	}

	// 404 → resource gone → prune.
	if httpStatus == 404 {
		resp.State.RemoveResource(ctx)
		return
	}

	// Any other non-2xx → hard error.
	if httpStatus < 200 || httpStatus >= 300 {
		resp.Diagnostics.AddError(
			"TechZone API error during Read",
			fmt.Sprintf("GET /api/reservation/aws/%s returned HTTP %d. Body: %s",
				reservationID, httpStatus, truncate(body, 512)),
		)
		return
	}

	// 200 — decode and apply prune rules.
	var apiResp tzReservationResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		resp.Diagnostics.AddError("Failed to parse TechZone reservation response",
			fmt.Sprintf("GET /api/reservation/aws/%s: %s", reservationID, err.Error()))
		return
	}

	// Prune: terminal status (200 + "Deleted" or "Expired").
	statusStr := derefString(apiResp.Status)
	if techzone.IsTerminalStatus(statusStr) {
		resp.State.RemoveResource(ctx)
		return
	}

	// Prune: past provisionUntil.
	provisionUntil := derefString(apiResp.ProvisionUntil)
	if techzone.PastExpiry(provisionUntil, time.Now()) {
		resp.State.RemoveResource(ctx)
		return
	}

	// Live reservation — update state.
	newState := mapResponseToModel(state, &apiResp)
	resp.Diagnostics.Append(resp.State.Set(ctx, newState)...)
}

// ---------------------------------------------------------------------------
// Update — operational-only no-op (zero HTTP calls)
// ---------------------------------------------------------------------------

func (r *reservationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// All identity attributes are RequiresReplace, so Update is only reached for
	// operational changes (timeout_minutes, reservation_duration_days).
	// Contract (RFC §3.3 / Sho #9): MUST make ZERO TechZone API calls.
	var plan reservationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var state reservationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Copy operational values from plan into state; leave all other fields unchanged.
	state.TimeoutMinutes = plan.TimeoutMinutes
	state.ReservationDurationDays = plan.ReservationDurationDays

	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

// ---------------------------------------------------------------------------
// Delete — idempotent
// ---------------------------------------------------------------------------

func (r *reservationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if r.client == nil {
		// No client — nothing to delete.
		return
	}

	var state reservationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	reservationID := state.ID.ValueString()
	if reservationID == "" {
		// Empty ID in state → nothing to delete (idempotent).
		return
	}

	// Build the delete payload (from delete.sh):
	// { IBMID: <user_email>, requestType: "aws", reservationId: <id> }
	deletePayload, err := json.Marshal(map[string]string{
		"IBMID":         state.UserEmail.ValueString(),
		"requestType":   "aws",
		"reservationId": reservationID,
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to build delete payload", err.Error())
		return
	}

	httpStatus, body, err := r.client.DoDelete(ctx, "/api/reservation/aws/"+reservationID, deletePayload)
	if err != nil {
		// Transport failure — still consider this a best-effort delete.
		// Log but do not fail: the resource will be removed from state.
		resp.Diagnostics.AddWarning("TechZone delete transport error",
			fmt.Sprintf("DELETE /api/reservation/aws/%s: %s. Removing from state.", reservationID, err.Error()))
		return
	}

	// 200, 204, 404 all mean "gone" — idempotent success.
	if httpStatus == 200 || httpStatus == 204 || httpStatus == 404 {
		return
	}

	// Any other status is an error.
	resp.Diagnostics.AddError(
		"TechZone reservation delete failed",
		fmt.Sprintf("DELETE /api/reservation/aws/%s returned HTTP %d. Body: %s",
			reservationID, httpStatus, truncate(body, 512)),
	)
}

// ---------------------------------------------------------------------------
// ImportState
// ---------------------------------------------------------------------------

func (r *reservationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// mapResponseToModel maps a tzReservationResponse onto a reservationModel,
// preserving the input-attribute values from base and filling computed fields
// from the API response.
//
// Date sourcing (RFC §3.2, gotchas/techzone.md):
//   - start_date: provisionDate → start → startDate → ""
//   - end_date:   provisionUntil → end → endDate → ""
// The fallback chain is identical to the jq filters in create.sh and read.sh.
func mapResponseToModel(base reservationModel, r *tzReservationResponse) reservationModel {
	out := base

	out.ID = types.StringValue(r.ID)
	out.Status = types.StringValue(derefString(r.Status))

	// Date sourcing: provisionDate / provisionUntil are the real fields;
	// start / end / startDate / endDate are always null in TechZone responses.
	out.StartDate = types.StringValue(firstNonEmpty(
		derefString(r.ProvisionDate),
		derefString(r.Start),
		derefString(r.StartDate),
	))
	out.EndDate = types.StringValue(firstNonEmpty(
		derefString(r.ProvisionUntil),
		derefString(r.End),
		derefString(r.EndDate),
	))

	// service_links: decode serviceLinks array (default []).
	links := make([]ServiceLinkModel, 0, len(r.ServiceLinks))
	for _, sl := range r.ServiceLinks {
		links = append(links, ServiceLinkModel{
			Type: types.StringValue(sl.Type),
			URL:  types.StringValue(sl.URL),
		})
	}
	listVal, diags := types.ListValueFrom(context.Background(), types.ObjectType{AttrTypes: serviceLinkAttrTypes}, links)
	if diags.HasError() {
		// Fallback to empty list on mapping error (should not happen for valid input).
		listVal, _ = types.ListValueFrom(context.Background(), types.ObjectType{AttrTypes: serviceLinkAttrTypes}, []ServiceLinkModel{})
	}
	out.ServiceLinks = listVal

	return out
}

// derefString returns *s if s is non-nil, otherwise "".
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// firstNonEmpty returns the first non-empty string from the candidates,
// or "" if all are empty. Mirrors the jq `// ""` fallback chain.
func firstNonEmpty(candidates ...string) string {
	for _, c := range candidates {
		if c != "" {
			return c
		}
	}
	return ""
}

// truncate limits a byte slice to at most n bytes for use in error messages.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
