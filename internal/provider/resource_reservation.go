package provider

// Token-safety contract (RFC §4, Ei F-02): the api_key is accessed only via
// the *techzone.Client, which passes it exclusively as an Authorization header
// value.  It is never formatted into logs, diagnostics, or error strings here.
// No httputil.DumpRequest/DumpResponse anywhere in this file.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/float64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/shoootyou-ext/terraform-provider-ibmtechzone/internal/techzone"
)

// Ensure reservationResource satisfies the resource.Resource interface.
var _ resource.Resource = &reservationResource{}
var _ resource.ResourceWithImportState = &reservationResource{}

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// reservationResource implements the ibmtechzone_reservation managed resource.
type reservationResource struct {
	pd  *providerData
	now func() time.Time // injectable clock; defaults to time.Now (audit fix H3)
}

// reservationModel is the Terraform state model for ibmtechzone_reservation.
type reservationModel struct {
	// Identity inputs (RequiresReplace)
	TemplateVariables types.Map    `tfsdk:"template_variables"`
	Region            types.String `tfsdk:"region"`
	ReservationName   types.String `tfsdk:"reservation_name"`
	Purpose           types.String `tfsdk:"purpose"`
	CollectionID      types.String `tfsdk:"collection_id"`
	UserEmail         types.String `tfsdk:"user_email"`
	RequesterContext  types.Object `tfsdk:"requester_context"`

	// Operational inputs (no RequiresReplace)
	ReservationDurationDays types.Int64   `tfsdk:"reservation_duration_days"`
	TimeoutMinutes          types.Int64   `tfsdk:"timeout_minutes"`
	ExtensionWindowFraction types.Float64 `tfsdk:"extension_window_fraction"`

	// Computed outputs
	ID           types.String `tfsdk:"id"`
	Status       types.String `tfsdk:"status"`
	ServiceLinks types.List   `tfsdk:"service_links"`
	StartDate    types.String `tfsdk:"start_date"`
	EndDate      types.String `tfsdk:"end_date"`
	ExtendCount  types.Int64  `tfsdk:"extend_count"`
}

// RequesterContextModel is the nested struct for the requester_context attribute.
// Decoded from types.Object via basetypes.ObjectAsOptions.
type RequesterContextModel struct {
	Opportunity types.List   `tfsdk:"opportunity"`
	IUI         types.String `tfsdk:"iui"`
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
	ID             string      `json:"id"`
	Status         *string     `json:"status"`
	ServiceLinks   []tzSvcLink `json:"serviceLinks"`
	ProvisionDate  *string     `json:"provisionDate"`
	Start          *string     `json:"start"`
	StartDate      *string     `json:"startDate"`
	ProvisionUntil *string     `json:"provisionUntil"`
	End            *string     `json:"end"`
	EndDate        *string     `json:"endDate"`
	ExtendCount    *int64      `json:"extendCount"` // confirmed present on every GET/create response
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
// Extension-window: payload builder, 400-response classifier, validator
// ---------------------------------------------------------------------------

// tzExtensionRejection decodes the subset of a 400 response body relevant to
// the extension-eligibility decision (idea sessions 3/4/6).
type tzExtensionRejection struct {
	Error  string   `json:"error"`
	Errors []string `json:"errors"`
	Policy struct {
		IsExtendable *bool `json:"isExtendable"`
	} `json:"policy"`
}

// tzExtensionSuccessResponse decodes the subset of a 2xx extension-POST
// response body relevant to confirming a genuine success shape (idea
// sessions 3/6: {"message":"ok","status":200}). classifyExtensionResponse
// requires at least one of these two marker fields to be present (Status
// equal to 200, or a non-empty Message) before trusting a 2xx HTTP status
// code — audit round-1 finding #5: without this check, an HTML body, an
// empty body, or any other unexpected shape arriving with a 2xx status
// silently resolved to extensionSucceeded.
type tzExtensionSuccessResponse struct {
	Message string `json:"message"`
	Status  int    `json:"status"`
}

// extensionOutcome classifies a completed extension POST attempt.
type extensionOutcome int

const (
	extensionUnknown extensionOutcome = iota // zero value never returned — a stray zero-value here is a bug
	extensionSucceeded
	extensionNotPossible // expected, evidenced outcome — fall through to prune/recreate, log at Info
	extensionAmbiguous   // unrecognized shape — fall through to prune/recreate, but log at Warn (see spec §6)
)

// buildExtensionPayload constructs the JSON body for POST /api/reservation/aws/<id>
// (the confirmed extension mechanism, idea sessions 2/3/6). Wire fields:
//
//	IBMID:         reservation owner's email — same source as the delete
//	               payload's IBMID (state.UserEmail).
//	requestType:   always "aws".
//	extensionDate: new ABSOLUTE provisionUntil (ISO-8601, NOT a delta —
//	               confirmed empirically). See techzone.NextExtensionDate.
//	reservationId: the reservation ID.
//	id:            duplicate of reservationId — confirmed required by the
//	               real API alongside reservationId.
func buildExtensionPayload(userEmail, reservationID, extensionDate string) ([]byte, error) {
	return json.Marshal(map[string]string{
		"IBMID":         userEmail,
		"requestType":   "aws",
		"extensionDate": extensionDate,
		"reservationId": reservationID,
		"id":            reservationID,
	})
}

// classifyExtensionResponse decides the outcome of a completed POST
// /api/reservation/aws/<id> extension attempt from its HTTP status and body.
//
// Rules (confirmed by 6 empirical research sessions,
// .yui-soul/ideas/terraform-provider-ibmtechzone.md):
//   - status in [200,300) AND the body decodes into tzExtensionSuccessResponse
//     with either Status==200 or a non-empty Message (the confirmed success
//     shape, {"message":"ok","status":200}): extensionSucceeded. The body
//     carries no usable reservation data beyond this shape check — the
//     caller derives new state from the request it just sent, not from this
//     response. A 2xx status whose body does NOT match this minimal shape
//     (empty body, HTML, or any other unexpected form) falls through to
//     extensionAmbiguous instead of being trusted on status code alone
//     (audit round-1 finding #5 — the same fail-safe pattern already applied
//     to the 400 path below).
//   - status == 400 AND body decodes with policy.isExtendable != nil AND
//     *policy.isExtendable == false: extensionNotPossible. This is the
//     confirmed-universal signal across BOTH documented rejection patterns:
//   - limit exhausted:        errors:[]                      + validation.extension:true
//   - never extendable (limit 0 from creation): errors:["Invalid extension date"] + validation.extension:false
//     Deciding on policy.isExtendable ALONE — never on errors[] contents or
//     the error string, which vary by sub-case (idea sessions 3/4/6).
//   - Anything else (malformed JSON, policy.isExtendable absent/null,
//     unexpected status code) → extensionAmbiguous. Caller MUST log this
//     (spec §6) — this is the concrete fix for the fail-silent-to-KEEP
//     pattern: an unrecognized shape must be OBSERVABLE, not silently
//     equivalent to a clean rejection.
func classifyExtensionResponse(status int, body []byte) extensionOutcome {
	if status >= 200 && status < 300 {
		var ok2xx tzExtensionSuccessResponse
		if err := json.Unmarshal(body, &ok2xx); err == nil && (ok2xx.Status == 200 || ok2xx.Message != "") {
			return extensionSucceeded
		}
		return extensionAmbiguous
	}
	if status == 400 {
		var rej tzExtensionRejection
		if err := json.Unmarshal(body, &rej); err == nil &&
			rej.Policy.IsExtendable != nil && !*rej.Policy.IsExtendable {
			return extensionNotPossible
		}
	}
	return extensionAmbiguous
}

// extensionWindowFractionValidator enforces that extension_window_fraction,
// multiplied by reservation_duration_days, never exceeds
// reservation_duration_days itself — equivalent to requiring
// extension_window_fraction <= 1.0 given the current "increment = full
// duration" design (spec §2), but computed and reported against both real
// sibling values so the error message is concrete, not just an abstract
// bound (Q1, user decision 2026-07-15).
//
// KNOWN LIMITATION (accepted — see spec "Known limitation" section): if
// reservation_duration_days is not set in the user's config (relying on its
// own schema Default), this validator cannot read a resolved value for it —
// ValidateFloat64 runs against the RAW config, before defaults are ever
// applied. In that case this validator deliberately skips validation rather
// than guessing at a value that doesn't exist yet.
type extensionWindowFractionValidator struct{}

func (v extensionWindowFractionValidator) Description(_ context.Context) string {
	return "extension_window_fraction × reservation_duration_days must not exceed reservation_duration_days"
}

func (v extensionWindowFractionValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v extensionWindowFractionValidator) ValidateFloat64(ctx context.Context, req validator.Float64Request, resp *validator.Float64Response) {
	// Not set by the user at all (relying on the schema Default) — nothing
	// to validate against a value the user isn't choosing.
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	fraction := req.ConfigValue.ValueFloat64()

	var durationDays types.Int64
	diags := req.Config.GetAttribute(ctx, path.Root("reservation_duration_days"), &durationDays)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	// KNOWN LIMITATION (accepted, see spec): unset/unknown sibling means "it
	// resolves to its own Default later, not visible from here yet." Skip
	// rather than guess — see the dedicated spec section.
	if durationDays.IsNull() || durationDays.IsUnknown() {
		return
	}

	days := durationDays.ValueInt64()
	if days <= 0 {
		// Nonsensical, but validating reservation_duration_days itself is
		// not this validator's job — InExtensionWindow already fails closed
		// on durationDays<=0 at runtime regardless (spec §1).
		return
	}

	windowDays := fraction * float64(days)
	if windowDays > float64(days) {
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Invalid extension_window_fraction",
			fmt.Sprintf(
				"extension_window_fraction is %g, and reservation_duration_days is %d, so the "+
					"computed extension window would be %g days — longer than the reservation's "+
					"own %d-day duration. extension_window_fraction must be in the range (0, 1] so "+
					"that extension_window_fraction × reservation_duration_days never exceeds "+
					"reservation_duration_days.",
				fraction, days, windowDays, days,
			),
		)
	}
}

// ---------------------------------------------------------------------------
// Factory + metadata
// ---------------------------------------------------------------------------

// NewReservationResource is the factory function registered in Resources().
func NewReservationResource() resource.Resource {
	return &reservationResource{
		now: time.Now, // default injectable clock (audit fix H3)
	}
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
			"> **Note:** Identity attributes (`template_variables`, `region`, `reservation_name`, " +
			"`purpose`, `collection_id`, `user_email`) trigger replacement when changed. " +
			"`timeout_minutes` and `reservation_duration_days` are operational and do not " +
			"trigger replacement.",
		Attributes: map[string]schema.Attribute{
			// --- Identity inputs (RequiresReplace) ---
			"template_variables": schema.MapAttribute{
				MarkdownDescription: "Map of opaque `_NN_` output keys to string values " +
					"(TF attribute: `template_variables`). Injected into the reservation " +
					"payload as both a `dynamicOutputs` array (in lexicographic key order) " +
					"and as flat top-level keys (dual-emit). These map to the TechZone API's " +
					"`dynamicOutputs` wire field.\n\n" +
					"Keys follow the `_NN_name` convention (e.g. `_04_hcp_org`, " +
					"`_05_hcp_project`). An empty map (`{}`) is valid and results in " +
					"`\"dynamicOutputs\": []` with no flat keys.\n\n" +
					"Example:\n```hcl\ntemplate_variables = {\n  \"_04_hcp_org\"     = " +
					"\"my-hcp-org\"\n  \"_05_hcp_project\" = \"my-hcp-project\"\n}\n```",
				ElementType: types.StringType,
				Required:    true,
				PlanModifiers: []planmodifier.Map{
					mapplanmodifier.RequiresReplace(),
				},
			},
			"region": schema.StringAttribute{
				MarkdownDescription: "AWS region for the reservation. Defaults to `us-east-2`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("us-east-2"),
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"reservation_name": schema.StringAttribute{
				MarkdownDescription: "Name for the reservation.",
				Optional:            true,
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"purpose": schema.StringAttribute{
				MarkdownDescription: "Reservation purpose.",
				Optional:            true,
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"collection_id": schema.StringAttribute{
				MarkdownDescription: "TechZone collection ID for this reservation. Required. " +
					"Must be a 24-character hexadecimal string (MongoDB ObjectID format).",
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators: []validator.String{
					// Enforce strict MongoDB ObjectID format (Ei audit F-01): 24-char hex.
					// This matches the MarkdownDescription ("24-character hexadecimal string")
					// and all observed TechZone collection IDs. url.PathEscape in GetCollection
					// provides a second layer, but schema validation catches bad inputs at
					// plan time before any HTTP call is made.
					stringvalidator.RegexMatches(
						regexp.MustCompile(`^[a-fA-F0-9]{24}$`),
						"must be a 24-character hexadecimal string (MongoDB ObjectID format)",
					),
				},
			},
			"user_email": schema.StringAttribute{
				MarkdownDescription: "IBM ID (email) of the reservation owner. Required. " +
					"Also used as the `IBMID` in the delete payload.",
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"requester_context": schema.SingleNestedAttribute{
				MarkdownDescription: "Optional CRM/requester context for the reservation. " +
					"Changes trigger replacement. " +
					"`opportunity` must be a list of CRM opportunity IDs (emitted as a JSON array). " +
					"`iui` is an optional IUI string.",
				Optional: true,
				PlanModifiers: []planmodifier.Object{
					objectplanmodifier.RequiresReplace(),
				},
				Attributes: map[string]schema.Attribute{
					"opportunity": schema.ListAttribute{
						MarkdownDescription: "List of CRM opportunity IDs. Emitted as a JSON array in the create payload. " +
							"Sending a string instead of an array causes the live API to return HTTP 400.",
						Optional:    true,
						ElementType: types.StringType,
					},
					"iui": schema.StringAttribute{
						MarkdownDescription: "IUI (IBM Unique Identifier) string. Omitted from the payload when not set.",
						Optional:            true,
					},
				},
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
			"extension_window_fraction": schema.Float64Attribute{
				MarkdownDescription: "Fraction (0, 1] of `reservation_duration_days`, counted backward from " +
					"the reservation's `end_date`, during which the provider attempts an automatic extension " +
					"before falling back to recreation. Defaults to `0.5` (the last 50% of the reservation's " +
					"duration). Must satisfy `extension_window_fraction * reservation_duration_days <= " +
					"reservation_duration_days` (i.e. `extension_window_fraction <= 1.0`), so a single " +
					"successful extension always moves the reservation outside its own window.",
				Optional: true,
				Computed: true,
				Default:  float64default.StaticFloat64(techzone.DefaultExtensionWindowFraction),
				Validators: []validator.Float64{
					extensionWindowFractionValidator{},
				},
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
				// UseStateForUnknown prevents "Provider produced inconsistent result" on
				// operational-only Updates (audit fix H1 / Sho-A #1): the Framework would
				// mark status as "(known after apply)" when only timeout_minutes changes,
				// but Update copies state → state, so the Framework sees a plan mismatch.
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
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
				// UseStateForUnknown: same rationale as status (audit fix H1).
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"end_date": schema.StringAttribute{
				MarkdownDescription: "Reservation end date from `provisionUntil` (or fallback chain).",
				Computed:            true,
				// UseStateForUnknown: same rationale as status (audit fix H1).
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"extend_count": schema.Int64Attribute{
				MarkdownDescription: "Number of successful extensions applied to this reservation via the " +
					"provider's extension-window mechanism (wire field `extendCount`). Starts at `0`. Reset to " +
					"`0` when the resource is recreated (a new reservation is a new `extendCount` sequence).",
				Computed:      true,
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Configure — receive providerData from provider
// ---------------------------------------------------------------------------

func (r *reservationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		// Normal during the validate phase — no provider data yet.
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
	r.pd = pd
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

func (r *reservationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if r.pd == nil || r.pd.Client == nil {
		resp.Diagnostics.AddError("Provider not configured",
			"The provider has no api_key configured. Configure the provider with a valid TechZone API key.")
		return
	}
	// Token gate: Create is load-bearing — fail if token probe failed.
	if r.pd.TokenErr != nil {
		if r.pd.TokenErrIsConnectivity {
			resp.Diagnostics.AddError("Could not reach TechZone API", r.pd.TokenErr.Error())
		} else {
			resp.Diagnostics.AddError("TECHZONE_API_KEY is invalid or expired", r.pd.TokenErr.Error())
		}
		return
	}

	var plan reservationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Read template_variables (TF attr) from the plan into a plain Go map.
	// Wire: template_variables → dynamicOutputs[] array + flat _NN_ keys in the payload.
	var templateVariables map[string]string
	resp.Diagnostics.Append(plan.TemplateVariables.ElementsAs(ctx, &templateVariables, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Fetch the collection to derive platform/region/template/etc. fields.
	// Token-safety: never include the bearer token in error messages.
	c := r.pd.Client
	coll, err := c.GetCollection(ctx, plan.CollectionID.ValueString())
	if err != nil {
		switch {
		case errors.Is(err, techzone.ErrCollectionNotFound):
			resp.Diagnostics.AddError("Collection not found",
				fmt.Sprintf("Collection %q was not found in TechZone. Verify the collection_id is correct.",
					plan.CollectionID.ValueString()))
		case errors.Is(err, techzone.ErrCollectionUnavailable):
			resp.Diagnostics.AddError("Collection unavailable",
				"The TechZone collection is temporarily unavailable. Try again later.")
		case errors.Is(err, techzone.ErrMalformedCollectionResponse),
			errors.Is(err, techzone.ErrEmptyPlatforms),
			errors.Is(err, techzone.ErrNoRegions):
			resp.Diagnostics.AddError("Collection data incomplete (contact TechZone)",
				"The collection response is missing required platform or region data. "+
					"Contact TechZone support.")
		case errors.Is(err, techzone.ErrUnsupportedInfrastructure):
			resp.Diagnostics.AddError("Infrastructure type not supported",
				"The collection uses an infrastructure type that is not supported by this provider. "+
					"Only AWS collections are supported.")
		default:
			resp.Diagnostics.AddError("Failed to fetch collection",
				fmt.Sprintf("GET collection %q: %s", plan.CollectionID.ValueString(), err.Error()))
		}
		return
	}

	// Derive scalar fields from the primary platform's first region.
	primaryPlatform := coll.Platforms[0]
	primaryRegion := primaryPlatform.Regions[0]

	// Compute start/end timestamps: start = now+1min, end = now+1min+duration_days.
	now := r.now().UTC().Truncate(time.Minute)
	durationDays := plan.ReservationDurationDays.ValueInt64()

	start := now.Add(time.Minute).Format("2006-01-02T15:04:05.000Z")
	end := now.Add(time.Duration(durationDays) * 24 * time.Hour).Format("2006-01-02T15:04:05.000Z")

	input := techzone.CreateInput{
		Name:          plan.ReservationName.ValueString(),
		Purpose:       plan.Purpose.ValueString(),
		Region:        primaryRegion.Region,
		Datacenter:    primaryRegion.Datacenter,
		CollectionID:  plan.CollectionID.ValueString(),
		Template:      primaryRegion.Template,
		RequestMethod: primaryRegion.RequestMethod,
		CloudAccount:  primaryRegion.CloudAccount,
		Start:         start,
		End:           end,
		// Wire user_email → payload "user" (Plan 114 live-validation fix).
		// Live API returns HTTP 500 "Invalid user assignment" when "user" is absent.
		User: plan.UserEmail.ValueString(),
	}

	// Wire requester_context → Opportunity + IUI (Plan 114 live-validation fix).
	// Guard null/unknown before extracting: the block is optional and may not be set.
	if !plan.RequesterContext.IsNull() && !plan.RequesterContext.IsUnknown() {
		var rc RequesterContextModel
		resp.Diagnostics.Append(plan.RequesterContext.As(ctx, &rc, basetypes.ObjectAsOptions{})...)
		if resp.Diagnostics.HasError() {
			return
		}
		if !rc.Opportunity.IsNull() && !rc.Opportunity.IsUnknown() {
			var opps []string
			resp.Diagnostics.Append(rc.Opportunity.ElementsAs(ctx, &opps, false)...)
			if resp.Diagnostics.HasError() {
				return
			}
			input.Opportunity = opps
		}
		if !rc.IUI.IsNull() && !rc.IUI.IsUnknown() {
			input.IUI = rc.IUI.ValueString()
		}
	}

	payload, err := techzone.BuildCreatePayload(primaryPlatform.Raw, templateVariables, input)
	if err != nil {
		resp.Diagnostics.AddError("Failed to build create payload", err.Error())
		return
	}

	// POST /api/reservation/aws
	status, body, err := r.pd.Client.DoPost(ctx, "/api/reservation/aws", payload)
	if err != nil {
		resp.Diagnostics.AddError("TechZone API unreachable", fmt.Sprintf("POST /api/reservation/aws: %s", err.Error()))
		return
	}
	// 401/403/302 → auth failure — suppress body to avoid leaking SSO redirect
	// HTML. Same guard as Read()'s initial GET, the poll loop, Final GET, and
	// Delete() (r3 audit finding 1 — this was the last unguarded call site
	// sharing the identical risk pattern: providerData.TokenErr is probed once
	// at Configure() time and only read, never re-validated, on every
	// subsequent Create() call, so a token can expire mid-apply just as it can
	// for the other 5 already-guarded sites). Falling through to the generic
	// non-2xx branch below would otherwise embed up to 512 raw response bytes
	// directly into resp.Diagnostics — the channel always visible to the user
	// on every plan/apply/refresh, no TF_LOG required.
	if status == 401 || status == 403 || status == 302 {
		resp.Diagnostics.AddError(
			"TECHZONE_API_KEY is invalid or expired",
			"TECHZONE_API_KEY is invalid or expired. Refresh it at https://techzone.ibm.com and re-run.",
		)
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

	// Poll loop: GET /api/reservation/aws/<id> every 10s until Ready or terminal.
	// Poll starts at T+0: first GET immediately, before the first ticker tick
	// (audit fix Sho-A #2 / medium finding #5).
	timeoutMinutes := plan.TimeoutMinutes.ValueInt64()
	maxAttempts := timeoutMinutes * 6 // 6 × 10s = 60s per minute
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// attempt counts from 0; first iteration fires immediately (T+0 poll).
	for attempt := int64(0); attempt < maxAttempts; attempt++ {
		// On subsequent attempts, wait for the next tick.
		if attempt > 0 {
			select {
			case <-ctx.Done():
				resp.Diagnostics.AddError("Context cancelled", "Terraform interrupted while waiting for reservation to become Ready.")
				return
			case <-ticker.C:
			}
		}

		// Poll path: /api/reservation/aws/<id> — returns status directly (no redirect).
		// The legacy /api/reservation/<id> returns HTTP 302, which the client sees raw
		// (redirect-following is disabled for SSO-token-expiry detection) and treats as
		// non-2xx, causing the poll loop to retry forever. Use the typed endpoint instead.
		pollStatus, pollBody, pollErr := r.pd.Client.DoGet(ctx, "/api/reservation/aws/"+url.PathEscape(reservationID))
		if pollErr != nil {
			tflog.Warn(ctx, "Poll connectivity error, retrying", map[string]any{
				"reservation_id": reservationID,
				"attempt":        attempt,
				"error":          pollErr.Error(),
			})
			continue
		}
		if pollStatus < 200 || pollStatus >= 300 {
			// Suppress body_preview at 401/403/302 (r2 finding 1): auth-rejection
			// bodies may reflect credential material or SSO redirect HTML. Log
			// only the status code for these cases.
			logFields := map[string]any{
				"reservation_id": reservationID,
				"attempt":        attempt,
				"http_status":    pollStatus,
			}
			if pollStatus != 401 && pollStatus != 403 && pollStatus != 302 {
				logFields["body_preview"] = truncate(pollBody, 200)
			}
			tflog.Warn(ctx, "Poll returned non-2xx status, retrying", logFields)
			continue
		}

		var pollResp tzPollResponse
		if err := json.Unmarshal(pollBody, &pollResp); err != nil {
			tflog.Warn(ctx, "Poll response unmarshal error, retrying", map[string]any{
				"reservation_id":  reservationID,
				"attempt":         attempt,
				"unmarshal_error": err.Error(),
			})
			continue
		}
		if pollResp.Status == nil {
			tflog.Warn(ctx, "Poll response missing status field, retrying", map[string]any{
				"reservation_id": reservationID,
				"attempt":        attempt,
				"body_preview":   truncate(pollBody, 200),
			})
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
			goto pollDone
		default:
			tflog.Trace(ctx, "Poll returned non-terminal status, continuing", map[string]any{
				"reservation_id": reservationID,
				"attempt":        attempt,
				"status":         s,
			})
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
		canStatus, canBody, canErr := r.pd.Client.DoGet(ctx, "/api/reservation/aws/"+url.PathEscape(reservationID))
		if canErr != nil {
			tflog.Warn(ctx, "Final GET connectivity error, retrying", map[string]any{
				"reservation_id": reservationID,
				"attempt":        attempt,
				"error":          canErr.Error(),
			})
			select {
			case <-ctx.Done():
				resp.Diagnostics.AddError("Context cancelled", "Interrupted during final read after reservation became Ready.")
				return
			case <-ticker.C:
			}
			continue
		}
		if canStatus < 200 || canStatus >= 300 {
			// Suppress body_preview at 401/403/302 (r2 finding 1) — same guard
			// as the poll loop above. Auth-rejection bodies may contain SSO
			// redirect HTML or session metadata.
			finalLogFields := map[string]any{
				"reservation_id": reservationID,
				"attempt":        attempt,
				"http_status":    canStatus,
			}
			if canStatus != 401 && canStatus != 403 && canStatus != 302 {
				finalLogFields["body_preview"] = truncate(canBody, 200)
			}
			tflog.Warn(ctx, "Final GET returned non-2xx status, retrying", finalLogFields)
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
			tflog.Warn(ctx, "Final GET unmarshal error, retrying", map[string]any{
				"reservation_id":  reservationID,
				"attempt":         attempt,
				"unmarshal_error": err.Error(),
			})
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
	if r.pd == nil || r.pd.Client == nil {
		return
	}
	// Token gate: Read is NOT load-bearing for the destroy path. When the token
	// is expired, we return silently (leaving state unchanged) so Terraform can
	// proceed to plan and execute the Delete. Delete tolerates an expired token
	// (RFC §3.3/§4 M1). If we AddError here, destroy is aborted before Delete
	// runs — that is the bug this fix corrects (audit fix H2 / Shin F-2).
	if r.pd.TokenErr != nil {
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

	httpStatus, body, err := r.pd.Client.DoGet(ctx, "/api/reservation/aws/"+url.PathEscape(reservationID))
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

	// 401/403/302 → auth failure — suppress body to avoid leaking SSO redirect
	// HTML. Same guard as the poll loop, Final GET, and Delete paths (r2
	// finding 1 — this branch previously omitted 302, which fell through to
	// the generic non-2xx branch below and leaked the full response body,
	// up to 512 bytes, directly into resp.Diagnostics — a channel always
	// visible to the user on every plan/apply/refresh, no TF_LOG required).
	if httpStatus == 401 || httpStatus == 403 || httpStatus == 302 {
		resp.Diagnostics.AddError(
			"TECHZONE_API_KEY is invalid or expired",
			"TECHZONE_API_KEY is invalid or expired. Refresh it at https://techzone.ibm.com and re-run.",
		)
		return
	}

	// Any other non-2xx → hard error with body excerpt.
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

	provisionUntil := derefString(apiResp.ProvisionUntil)

	// --- Extension-window attempt (at most once per Read() call; no
	// in-process retry loop — a failed/ambiguous attempt is retried, if at
	// all, on the NEXT Terraform refresh cycle, keeping Read() bounded like
	// today, unlike Create()'s poll loop). Never calls Diagnostics.AddError —
	// extension is a best-effort optimization in front of the proven
	// recreate fallback; only tflog is used (spec §5/§6).
	durationDays := state.ReservationDurationDays.ValueInt64()
	windowFraction := state.ExtensionWindowFraction.ValueFloat64()
	eligible, ok := techzone.InExtensionWindow(provisionUntil, r.now(), durationDays, windowFraction)
	switch {
	case !ok:
		// Cannot determine (unparseable provisionUntil / durationDays<=0).
		// Must log, not silently skip (spec §6).
		tflog.Warn(ctx, "Could not determine extension-window eligibility, skipping extension attempt", map[string]any{
			"reservation_id": reservationID, "provision_until": provisionUntil, "duration_days": durationDays,
		})
	case eligible:
		newExtensionDate, dateOK := techzone.NextExtensionDate(provisionUntil, durationDays)
		if !dateOK {
			tflog.Warn(ctx, "Extension window eligible but could not compute extensionDate, skipping extension attempt", map[string]any{
				"reservation_id": reservationID,
			})
			break
		}
		payload, buildErr := buildExtensionPayload(state.UserEmail.ValueString(), reservationID, newExtensionDate)
		if buildErr != nil {
			tflog.Warn(ctx, "Failed to build extension payload, skipping extension attempt", map[string]any{
				"reservation_id": reservationID, "error": buildErr.Error(),
			})
			break
		}
		extStatus, extBody, extErr := r.pd.Client.DoPost(ctx, "/api/reservation/aws/"+url.PathEscape(reservationID), payload)
		if extErr != nil {
			tflog.Warn(ctx, "Extension POST transport error, falling back to existing prune logic", map[string]any{
				"reservation_id": reservationID, "error": extErr.Error(),
			})
			break
		}
		switch classifyExtensionResponse(extStatus, extBody) {
		case extensionSucceeded:
			// body_preview logs the VALIDATED struct fields (message, status),
			// NOT raw body bytes (r2 finding 2). classifyExtensionResponse's
			// shape check only confirms Status==200 or a non-empty Message —
			// it does not reject unexpected additional fields (Go's
			// json.Unmarshal ignores unknown keys). Logging truncate(extBody,
			// 200) would reflect the body's FULL raw content verbatim,
			// including any such unexpected field, exceeding what was
			// actually validated. Re-decoding here (rather than threading the
			// struct through classifyExtensionResponse's return signature)
			// keeps that function's signature and existing test suite
			// unchanged; the decode below is guaranteed to succeed
			// identically to the one classifyExtensionResponse already
			// performed on this exact body to reach this branch.
			var ok2xx tzExtensionSuccessResponse
			_ = json.Unmarshal(extBody, &ok2xx) // guaranteed success — see comment above
			// Guarded with the same 401/403/302 check as finding #1, applied
			// here for consistency — defense in depth only, since a genuine
			// extensionSucceeded classification can never itself carry a
			// 401/403/302 status (classifyExtensionResponse only returns it
			// for status in [200,300)).
			succeededLogFields := map[string]any{
				"reservation_id": reservationID, "new_provision_until": newExtensionDate,
			}
			if extStatus != 401 && extStatus != 403 && extStatus != 302 {
				succeededLogFields["body_preview"] = fmt.Sprintf("message=%q status=%d", ok2xx.Message, ok2xx.Status)
			}
			tflog.Info(ctx, "Reservation extended", succeededLogFields)
			newState := mapResponseToModel(state, &apiResp)
			newState.EndDate = types.StringValue(newExtensionDate)
			newState.ExtendCount = types.Int64Value(derefInt64(apiResp.ExtendCount) + 1)
			resp.Diagnostics.Append(resp.State.Set(ctx, newState)...)
			return // skip PastExpiry — reservation is now known-live
		case extensionNotPossible:
			tflog.Info(ctx, "Extension not possible (policy.isExtendable=false), falling back to existing prune logic", map[string]any{
				"reservation_id": reservationID, "http_status": extStatus,
			})
			// fall through — unchanged
		default: // extensionAmbiguous
			// Suppress body_preview at 401/403/302 — same guard already
			// applied by the poll loop, Final GET, and Delete paths (Ei R2
			// NF-01). A token can be invalidated in the window between the
			// initial GET (already successful) and this POST, or the
			// extension endpoint may require an elevated role and return
			// 403; a raw 302 can also reach here (the client never follows
			// redirects) and may carry SSO redirect HTML or session
			// metadata. Log only reservation_id + http_status for these
			// three cases (audit round-1 finding #1).
			ambiguousLogFields := map[string]any{
				"reservation_id": reservationID, "http_status": extStatus,
			}
			if extStatus != 401 && extStatus != 403 && extStatus != 302 {
				ambiguousLogFields["body_preview"] = truncate(extBody, 200)
			}
			tflog.Warn(ctx, "Extension response could not be classified, falling back to existing prune logic", ambiguousLogFields)
			// fall through — unchanged
		}
	}
	// case ok && !eligible (the common case — not yet near expiry): no log,
	// by design. This is the routine outcome for the vast majority of Read()
	// calls; logging it would be noise, not signal.
	// --- end extension-window attempt ---

	// Prune: past provisionUntil (audit fix H3: use injected clock r.now()).
	// Evaluated against the ORIGINAL provisionUntil in every fall-through
	// path above.
	if techzone.PastExpiry(provisionUntil, r.now()) {
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
	state.ExtensionWindowFraction = plan.ExtensionWindowFraction // spec §5 — no RequiresReplace, so an
	// edit to this attribute must not be silently discarded (Update has no
	// generic "copy all operational fields" loop to piggyback on).

	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

// ---------------------------------------------------------------------------
// Delete — idempotent; tolerates expired/invalid token (audit fix H2, M4)
// ---------------------------------------------------------------------------

func (r *reservationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	// Delete MUST proceed even if the token probe failed (RFC §3.3/§4 M1).
	// We only need a client — TokenErr is intentionally ignored here.
	if r.pd == nil || r.pd.Client == nil {
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

	httpStatus, body, err := r.pd.Client.DoDelete(ctx, "/api/reservation/aws/"+url.PathEscape(reservationID), deletePayload)
	if err != nil {
		// True transport failure (non-nil err with no usable HTTP response) →
		// AddError (audit fix Sho-A #3 / medium finding #4).
		// Note: http.ErrUseLastResponse can yield a non-nil err WITH a non-nil
		// resp — the DoDelete implementation returns (status, body, nil) in that
		// case, so err != nil here genuinely means no usable response was obtained.
		resp.Diagnostics.AddError(
			"TechZone delete transport error",
			fmt.Sprintf("DELETE /api/reservation/aws/%s: %s", reservationID, err.Error()),
		)
		return
	}

	// 200, 204, 404 → idempotent success (reservation gone or never existed).
	if httpStatus == 200 || httpStatus == 204 || httpStatus == 404 {
		return
	}

	// 302, 401, 403 → auth failure (expired/invalid token).
	// The bearer token MUST NOT appear in this message (RFC §4, Ei F-02).
	//
	// State is intentionally NOT modified here. The Framework only auto-clears
	// state on a clean (no-error) delete; when Diagnostics.HasError() the state
	// written into DeleteResponse is preserved as-is. Leaving state intact allows
	// the operator to refresh their token and retry — without having to re-import
	// the resource first.
	if httpStatus == 302 || httpStatus == 401 || httpStatus == 403 {
		resp.Diagnostics.AddError(
			"TECHZONE_API_KEY is invalid or expired",
			"TECHZONE_API_KEY is invalid or expired. Refresh it at https://techzone.ibm.com and re-run.",
		)
		return
	}

	// Any other non-2xx → generic error with the HTTP status code.
	resp.Diagnostics.AddError(
		"TechZone reservation delete failed",
		fmt.Sprintf("DELETE /api/reservation/aws/%s: delete failed: HTTP %d. Body: %s",
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
//
// The fallback chain is identical to the jq filters in create.sh and read.sh.
func mapResponseToModel(base reservationModel, r *tzReservationResponse) reservationModel {
	out := base

	// Normalize Optional+Computed fields: if the plan value is Unknown (not set by
	// the user and not yet resolved), replace with a known empty string so that the
	// Framework can accept the state after apply.  The API does not return these
	// fields, so "" is the correct resolved value when omitted from config.
	//
	// Note: `region` is not normalized here because it has a static default
	// ("us-east-2") — it is always a known value by the time Create is called.
	if out.ReservationName.IsUnknown() {
		out.ReservationName = types.StringValue("")
	}
	if out.Purpose.IsUnknown() {
		out.Purpose = types.StringValue("")
	}

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

	out.ExtendCount = types.Int64Value(derefInt64(r.ExtendCount))

	return out
}

// derefString returns *s if s is non-nil, otherwise "".
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// derefInt64 returns *n if n is non-nil, otherwise 0.
func derefInt64(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
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
