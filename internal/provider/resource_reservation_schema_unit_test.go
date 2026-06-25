/**
 * @spec-handoff
 *
 * @interface reservationResource.Schema — ibmtechzone_reservation schema (post-Plan-114 live fix)
 *
 * @behavior
 *   - `template`    attribute MUST NOT exist in the schema (removed in E5).
 *   - `hcp_org`     attribute MUST NOT exist in the schema (removed in E5).
 *   - `hcp_project` attribute MUST NOT exist in the schema (removed in E5).
 *   - `template_variables` attribute MUST exist as a MapAttribute of element type StringType.
 *   - `template_variables` is Required with RequiresReplace, so that changes to
 *     the map trigger replacement (same lifecycle as the other identity attributes).
 *   - `requester_context` attribute MUST exist as an OPTIONAL SingleNestedAttribute.
 *     Its nested attributes:
 *       - `opportunity` (Optional, list-of-string) — CRM opportunity IDs, sent as a
 *         JSON array in the create payload; live API returns 400 without it when required.
 *       - `iui` (Optional, string) — IUI field passed verbatim when non-empty.
 *   - reservationModel gains a RequesterContext nested object field typed to match.
 *   - All other existing attributes (collection_id, user_email, region, reservation_name,
 *     purpose, reservation_duration_days, timeout_minutes, id, status, service_links,
 *     start_date, end_date) must remain present and unchanged.
 *
 * @edge-cases
 *   - Schema() must return zero diagnostics.
 *   - template_variables round-trips: a state containing {"_04_hcp_org": "org1",
 *     "_05_hcp_project": "proj1"} must deserialize into a types.Map with those
 *     exact entries (no loss, no mutation).
 *   - requester_context round-trips: opportunity list and iui string survive
 *     tfsdk.State Set → Get without loss.
 *   - requester_context absent (null) → model field is null/unknown, Create wires
 *     empty Opportunity and empty IUI into CreateInput.
 *
 * @see ./resource_reservation.go          (Kou implements schema changes here)
 * @see ../techzone/payload.go             (BuildCreatePayload — updated CreateInput)
 * @see .yui-soul/plans/wip/114-techzone-template-agnostic/e5-schema-resource-wiring.md
 */

// Package provider — internal schema unit tests for E5 schema + Plan 114 live fix.
//
// RED GATE (Plan 114 live-validation fix): tests FAIL against current resource_reservation.go:
//   (a) `requester_context` does not exist in the schema → TestReservationSchema_RequesterContext
//       fails immediately.
//   (b) reservationModel has no RequesterContext field → struct literal compile error.
//   (c) CreateInput.User does not exist / Opportunity is string not []string → compile error
//       in payload_golden_test.go (same RED batch).
//
// Goes GREEN when Kou adds requester_context to Schema() and RequesterContext to
// reservationModel, and updates CreateInput accordingly.
package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// ---------------------------------------------------------------------------
// Schema shape assertions
// ---------------------------------------------------------------------------

// TestReservationSchema_RemovedAttributes asserts that `template`, `hcp_org`, and
// `hcp_project` NO LONGER appear in the resource schema after the E5 refactor.
//
// RED: all three attributes are present in the current schema; this test will fail
// (FAIL: attribute "template" must not exist in schema, but it does) until Kou
// removes them from Schema().
func TestReservationSchema_RemovedAttributes(t *testing.T) {
	t.Parallel()

	s := resourceSchemaForDelete(t) // reuses the existing helper from delete unit tests

	removedAttrs := []string{"template", "hcp_org", "hcp_project"}
	for _, name := range removedAttrs {
		if _, ok := s.Attributes[name]; ok {
			t.Errorf("FAIL: attribute %q must NOT exist in the schema (removed in E5), but it does", name)
		}
	}
}

// TestReservationSchema_DynamicOutputsExists asserts that the `template_variables`
// attribute exists and is typed as a map of strings.
func TestReservationSchema_DynamicOutputsExists(t *testing.T) {
	t.Parallel()

	s := resourceSchemaForDelete(t)

	attr, ok := s.Attributes["template_variables"]
	if !ok {
		t.Fatal("FAIL: attribute \"template_variables\" is absent from schema; must be a MapAttribute(StringType)")
	}

	// It must be a MapAttribute — the underlying type must expose a map element type.
	mapAttr, ok := attr.(rschema.MapAttribute)
	if !ok {
		t.Fatalf("FAIL: \"template_variables\" is %T, want rschema.MapAttribute", attr)
	}

	// Element type must be StringType.
	if mapAttr.ElementType != types.StringType {
		t.Errorf("FAIL: template_variables.ElementType = %T (%v), want types.StringType",
			mapAttr.ElementType, mapAttr.ElementType)
	}
}

// TestReservationSchema_ExistingAttributesPreserved asserts that the non-removed,
// non-added attributes are still present and correctly typed after the E5 refactor.
//
// RED: the package does not compile, so this test cannot even run.
func TestReservationSchema_ExistingAttributesPreserved(t *testing.T) {
	t.Parallel()

	s := resourceSchemaForDelete(t)

	required := []string{
		"collection_id",
		"user_email",
		"region",
		"reservation_name",
		"purpose",
		"reservation_duration_days",
		"timeout_minutes",
		"id",
		"status",
		"service_links",
		"start_date",
		"end_date",
	}
	for _, name := range required {
		if _, ok := s.Attributes[name]; !ok {
			t.Errorf("FAIL: attribute %q must remain in schema after E5 but is absent", name)
		}
	}
}

// ---------------------------------------------------------------------------
// reservationModel round-trip: dynamic_outputs
// ---------------------------------------------------------------------------

// TestReservationModel_DynamicOutputs_RoundTrip builds a reservationModel that
// includes template_variables (the TF attribute) and serializes it into a tfsdk.State,
// then deserializes it back, asserting that the map is preserved.
func TestReservationModel_DynamicOutputs_RoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := resourceSchemaForDelete(t)

	// Build a types.Map with two entries, mirroring the DDR inputs.
	dynMap, diags := types.MapValue(types.StringType, map[string]attr.Value{
		"_04_hcp_org":     types.StringValue("org-test"),
		"_05_hcp_project": types.StringValue("proj-test"),
	})
	if diags.HasError() {
		t.Fatalf("building template_variables map: %v", diags)
	}

	// Build a minimal but valid empty ServiceLinks list.
	emptyLinks, diags := types.ListValueFrom(
		ctx,
		types.ObjectType{AttrTypes: serviceLinkAttrTypes},
		[]ServiceLinkModel{},
	)
	if diags.HasError() {
		t.Fatalf("building empty service_links: %v", diags)
	}

	// Build the new-contract model: no Template, HCPOrg, or HCPProject fields;
	// TemplateVariables field present (TF attribute: template_variables).
	//
	// E8 (Plan 114): RequesterContext added to schema — must be a typed null
	// object (not the zero-value types.Object{}) so that the Framework can
	// validate the attribute type against the schema.
	rcAttrTypesForDynTest := map[string]attr.Type{
		"opportunity": types.ListType{ElemType: types.StringType},
		"iui":         types.StringType,
	}
	m := reservationModel{
		TemplateVariables:       dynMap,
		Region:                  types.StringValue("us-east-2"),
		ReservationName:         types.StringValue("test-reservation"),
		Purpose:                 types.StringValue("Demo"),
		CollectionID:            types.StringValue("test-collection-id"),
		UserEmail:               types.StringValue("test@example.com"),
		RequesterContext:        types.ObjectNull(rcAttrTypesForDynTest),
		ReservationDurationDays: types.Int64Value(1),
		TimeoutMinutes:          types.Int64Value(30),
		ID:                      types.StringValue("res-123"),
		Status:                  types.StringValue("Ready"),
		ServiceLinks:            emptyLinks,
		StartDate:               types.StringValue("2026-01-01T00:00:01.000Z"),
		EndDate:                 types.StringValue("2026-01-02T00:01:01.000Z"),
	}

	// Serialize into tfsdk.State.
	rawType := s.Type().TerraformType(ctx)
	state := tfsdk.State{
		Schema: s,
		Raw:    tftypes.NewValue(rawType, nil),
	}
	if diags := state.Set(ctx, m); diags.HasError() {
		t.Fatalf("state.Set() failed: %v", diags)
	}

	// Deserialize back.
	var got reservationModel
	if diags := state.Get(ctx, &got); diags.HasError() {
		t.Fatalf("state.Get() failed: %v", diags)
	}

	// Assert template_variables round-tripped correctly.
	if got.TemplateVariables.IsNull() || got.TemplateVariables.IsUnknown() {
		t.Fatal("FAIL: template_variables is null/unknown after round-trip")
	}

	var gotElems map[string]types.String
	if diags := got.TemplateVariables.ElementsAs(ctx, &gotElems, false); diags.HasError() {
		t.Fatalf("ElementsAs failed: %v", diags)
	}

	wantElems := map[string]string{
		"_04_hcp_org":     "org-test",
		"_05_hcp_project": "proj-test",
	}
	for k, want := range wantElems {
		gotV, ok := gotElems[k]
		if !ok {
			t.Errorf("FAIL: template_variables[%q] absent after round-trip", k)
			continue
		}
		if gotV.ValueString() != want {
			t.Errorf("FAIL: template_variables[%q] = %q, want %q", k, gotV.ValueString(), want)
		}
	}
	if len(gotElems) != len(wantElems) {
		t.Errorf("FAIL: template_variables has %d entries after round-trip, want %d",
			len(gotElems), len(wantElems))
	}

	// Assert removed fields are GONE from the model (compile-time: if Template/HCPOrg/
	// HCPProject fields still exist on reservationModel, the struct literal above that
	// omits them would still compile — but the schema assertions in
	// TestReservationSchema_RemovedAttributes catch the schema side, and the delete
	// unit test helper below catches the model side by not populating those fields).
}

// ---------------------------------------------------------------------------
// buildDeleteState — updated to new reservationModel (no template/hcp_org/hcp_project)
// ---------------------------------------------------------------------------

// buildDeleteStateV2 is the E5-era replacement for buildDeleteState.
// It builds a tfsdk.State with the new reservationModel schema:
//   - No Template, HCPOrg, or HCPProject fields.
//   - TemplateVariables map(string) with two DDR entries (TF attr: template_variables).
//
// The existing TestDeleteUnit_* tests remain valid — they call buildDeleteState
// which was updated in-place to match the new model.
func buildDeleteStateV2(t *testing.T, s rschema.Schema, reservationID string) tfsdk.State {
	t.Helper()
	ctx := context.Background()

	rawType := s.Type().TerraformType(ctx)
	state := tfsdk.State{
		Schema: s,
		Raw:    tftypes.NewValue(rawType, nil),
	}

	emptyLinks, diags := types.ListValueFrom(
		ctx,
		types.ObjectType{AttrTypes: serviceLinkAttrTypes},
		[]ServiceLinkModel{},
	)
	if diags.HasError() {
		t.Fatalf("building empty service_links: %v", diags)
	}

	dynMap, diags := types.MapValue(types.StringType, map[string]attr.Value{
		"_04_hcp_org":     types.StringValue("test-hcp-org"),
		"_05_hcp_project": types.StringValue("test-hcp-project"),
	})
	if diags.HasError() {
		t.Fatalf("building template_variables map: %v", diags)
	}

	// New-contract model: no Template/HCPOrg/HCPProject; TemplateVariables present.
	// E8 (Plan 114): RequesterContext typed-null added to satisfy schema type
	// validation after requester_context attribute was added to the schema.
	rcAttrTypesForV2 := map[string]attr.Type{
		"opportunity": types.ListType{ElemType: types.StringType},
		"iui":         types.StringType,
	}
	m := reservationModel{
		TemplateVariables:       dynMap,
		Region:                  types.StringValue("us-east-2"),
		ReservationName:         types.StringValue("Reservation Name"),
		Purpose:                 types.StringValue("Demo"),
		CollectionID:            types.StringValue("test-collection-id"),
		UserEmail:               types.StringValue("test@example.com"),
		RequesterContext:        types.ObjectNull(rcAttrTypesForV2),
		ReservationDurationDays: types.Int64Value(1),
		TimeoutMinutes:          types.Int64Value(30),
		ID:                      types.StringValue(reservationID),
		Status:                  types.StringValue("Ready"),
		ServiceLinks:            emptyLinks,
		StartDate:               types.StringValue("2024-06-15T10:00:00Z"),
		EndDate:                 types.StringValue("2099-12-31T23:59:59Z"),
	}

	if diags := state.Set(ctx, m); diags.HasError() {
		t.Fatalf("state.Set() failed: %v", diags)
	}
	return state
}

// ---------------------------------------------------------------------------
// Plan 114 live-validation fix: requester_context schema + model tests
// ---------------------------------------------------------------------------

// TestReservationSchema_RequesterContextExists asserts that `requester_context`
// exists in the schema as an Optional SingleNestedAttribute with the correct
// nested attributes: `opportunity` (list-of-string, optional) and `iui` (string, optional).
//
// RED: `requester_context` does not exist in the current schema.
// Goes GREEN when Kou adds it to Schema() in resource_reservation.go.
func TestReservationSchema_RequesterContextExists(t *testing.T) {
	t.Parallel()

	s := resourceSchemaForDelete(t)

	raw, ok := s.Attributes["requester_context"]
	if !ok {
		t.Fatal("FAIL: `requester_context` attribute is absent from schema; " +
			"must be an Optional SingleNestedAttribute (Plan 114 live fix)")
	}

	nested, ok := raw.(rschema.SingleNestedAttribute)
	if !ok {
		t.Fatalf("FAIL: `requester_context` is %T, want rschema.SingleNestedAttribute", raw)
	}

	// Must be Optional (not Required, not Computed-only).
	if !nested.Optional {
		t.Errorf("FAIL: `requester_context` must be Optional (it is absent when not needed)")
	}

	// Nested: opportunity (list-of-string, optional).
	oppRaw, ok := nested.Attributes["opportunity"]
	if !ok {
		t.Fatal("FAIL: `requester_context.opportunity` nested attribute is absent; " +
			"must be an Optional ListAttribute of StringType")
	}
	oppList, ok := oppRaw.(rschema.ListAttribute)
	if !ok {
		t.Fatalf("FAIL: `requester_context.opportunity` is %T, want rschema.ListAttribute", oppRaw)
	}
	if oppList.ElementType != types.StringType {
		t.Errorf("FAIL: `requester_context.opportunity` ElementType = %T, want types.StringType",
			oppList.ElementType)
	}
	if !oppList.Optional {
		t.Errorf("FAIL: `requester_context.opportunity` must be Optional")
	}

	// Nested: iui (string, optional).
	iuiRaw, ok := nested.Attributes["iui"]
	if !ok {
		t.Fatal("FAIL: `requester_context.iui` nested attribute is absent; " +
			"must be an Optional StringAttribute")
	}
	iuiStr, ok := iuiRaw.(rschema.StringAttribute)
	if !ok {
		t.Fatalf("FAIL: `requester_context.iui` is %T, want rschema.StringAttribute", iuiRaw)
	}
	if !iuiStr.Optional {
		t.Errorf("FAIL: `requester_context.iui` must be Optional")
	}
}

// TestReservationModel_RequesterContext_RoundTrip builds a reservationModel with
// a non-nil RequesterContext (opportunity list + iui string set) and verifies
// it round-trips through tfsdk.State Set → Get without loss.
//
// RED:
//   (a) reservationModel has no RequesterContext field → struct literal compile error.
//   (b) `requester_context` absent from schema → state.Set() returns diagnostics error.
//
// Goes GREEN when Kou adds RequesterContext to reservationModel and requester_context
// to Schema().
func TestReservationModel_RequesterContext_RoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := resourceSchemaForDelete(t)

	dynMap, diags := types.MapValue(types.StringType, map[string]attr.Value{
		"_04_hcp_org": types.StringValue("org-rc-test"),
	})
	if diags.HasError() {
		t.Fatalf("building template_variables map: %v", diags)
	}

	emptyLinks, diags := types.ListValueFrom(
		ctx,
		types.ObjectType{AttrTypes: serviceLinkAttrTypes},
		[]ServiceLinkModel{},
	)
	if diags.HasError() {
		t.Fatalf("building empty service_links: %v", diags)
	}

	// Build a RequesterContext object value.
	// The attr types must match what the schema declares for requester_context.
	//
	// RED: RequesterContext field does not exist on reservationModel yet.
	// This struct literal causes a compile error until Kou adds the field.
	rcAttrTypes := map[string]attr.Type{
		"opportunity": types.ListType{ElemType: types.StringType},
		"iui":         types.StringType,
	}

	oppList, diags := types.ListValueFrom(ctx, types.StringType, []string{"006Ka000003kVEoIAM"})
	if diags.HasError() {
		t.Fatalf("building opportunity list: %v", diags)
	}

	rcObj, diags := types.ObjectValue(rcAttrTypes, map[string]attr.Value{
		"opportunity": oppList,
		"iui":         types.StringValue("test-iui"),
	})
	if diags.HasError() {
		t.Fatalf("building requester_context object: %v", diags)
	}

	m := reservationModel{
		TemplateVariables:       dynMap,
		Region:                  types.StringValue("us-east-2"),
		ReservationName:         types.StringValue("rc-test"),
		Purpose:                 types.StringValue("Demo"),
		CollectionID:            types.StringValue("69650af0758b9e41de66b6ae"),
		UserEmail:               types.StringValue("tester@example.com"),
		ReservationDurationDays: types.Int64Value(1),
		TimeoutMinutes:          types.Int64Value(30),
		ID:                      types.StringValue("rc-res-001"),
		Status:                  types.StringValue("Ready"),
		ServiceLinks:            emptyLinks,
		StartDate:               types.StringValue("2026-01-01T00:00:01.000Z"),
		EndDate:                 types.StringValue("2026-01-02T00:01:01.000Z"),
		RequesterContext:        rcObj,
	}

	rawType := s.Type().TerraformType(ctx)
	state := tfsdk.State{
		Schema: s,
		Raw:    tftypes.NewValue(rawType, nil),
	}
	if diags := state.Set(ctx, m); diags.HasError() {
		t.Fatalf("state.Set() with RequesterContext failed: %v", diags)
	}

	var got reservationModel
	if diags := state.Get(ctx, &got); diags.HasError() {
		t.Fatalf("state.Get() after RequesterContext round-trip failed: %v", diags)
	}

	// RequesterContext must survive the round-trip.
	if got.RequesterContext.IsNull() || got.RequesterContext.IsUnknown() {
		t.Fatal("FAIL: RequesterContext is null/unknown after round-trip; expected populated object")
	}

	// Extract nested attributes.
	rcAttrs := got.RequesterContext.Attributes()

	gotOpp, ok := rcAttrs["opportunity"]
	if !ok {
		t.Fatal("FAIL: opportunity absent from RequesterContext after round-trip")
	}
	oppListGot, ok := gotOpp.(types.List)
	if !ok {
		t.Fatalf("FAIL: opportunity is %T after round-trip, want types.List", gotOpp)
	}
	var oppElems []types.String
	if diags := oppListGot.ElementsAs(ctx, &oppElems, false); diags.HasError() {
		t.Fatalf("opportunity.ElementsAs failed: %v", diags)
	}
	if len(oppElems) != 1 || oppElems[0].ValueString() != "006Ka000003kVEoIAM" {
		t.Errorf("FAIL: opportunity after round-trip = %v, want [\"006Ka000003kVEoIAM\"]", oppElems)
	}

	gotIUI, ok := rcAttrs["iui"]
	if !ok {
		t.Fatal("FAIL: iui absent from RequesterContext after round-trip")
	}
	iuiStr, ok := gotIUI.(types.String)
	if !ok {
		t.Fatalf("FAIL: iui is %T after round-trip, want types.String", gotIUI)
	}
	if iuiStr.ValueString() != "test-iui" {
		t.Errorf("FAIL: iui after round-trip = %q, want %q", iuiStr.ValueString(), "test-iui")
	}
}

// TestReservationSchema_ExistingAttributesIncludeRequesterContext verifies that
// the Schema() still contains all expected attributes including the new
// requester_context alongside the pre-existing ones.
//
// RED: requester_context absent → this test's last check for it fails.
func TestReservationSchema_ExistingAttributesIncludeRequesterContext(t *testing.T) {
	t.Parallel()

	s := resourceSchemaForDelete(t)

	// All attributes that must be present post-live-fix.
	required := []string{
		"collection_id",
		"user_email",
		"region",
		"reservation_name",
		"purpose",
		"template_variables",
		"reservation_duration_days",
		"timeout_minutes",
		"id",
		"status",
		"service_links",
		"start_date",
		"end_date",
		"requester_context", // NEW — Plan 114 live fix
	}
	for _, name := range required {
		if _, ok := s.Attributes[name]; !ok {
			t.Errorf("FAIL: attribute %q must exist in schema but is absent", name)
		}
	}
}
