/**
 * @spec-handoff
 *
 * @interface reservationResource.Schema — techzone_reservation schema (post-E5 refactor)
 *
 * @behavior
 *   - `template`    attribute MUST NOT exist in the schema (removed in E5).
 *   - `hcp_org`     attribute MUST NOT exist in the schema (removed in E5).
 *   - `hcp_project` attribute MUST NOT exist in the schema (removed in E5).
 *   - `dynamic_outputs` attribute MUST exist as a MapAttribute of element type StringType.
 *   - `dynamic_outputs` is Optional+Computed with RequiresReplace, so that changes to
 *     the map trigger replacement (same lifecycle as the other identity attributes).
 *   - All other existing attributes (collection_id, user_email, region, reservation_name,
 *     purpose, reservation_duration_days, timeout_minutes, id, status, service_links,
 *     start_date, end_date) must remain present and unchanged.
 *
 * @edge-cases
 *   - Schema() must return zero diagnostics.
 *   - dynamic_outputs round-trips: a state containing {"_04_hcp_org": "org1",
 *     "_05_hcp_project": "proj1"} must deserialize into a types.Map with those
 *     exact entries (no loss, no mutation).
 *
 * @see ./resource_reservation.go          (Kou implements schema changes here)
 * @see ../techzone/payload.go             (BuildCreatePayload — new 3-arg signature)
 * @see .yui-soul/plans/wip/114-techzone-template-agnostic/e5-schema-resource-wiring.md
 */

// Package provider — internal schema unit tests for E5 schema refactor.
//
// RED GATE (E5 Task 1): these tests FAIL against the current resource_reservation.go
// because:
//   (a) resource_reservation.go does not compile — it calls the old 1-arg
//       BuildCreatePayload and references CreateInput.User/HCPOrg/HCPProject
//       which no longer exist on the struct.
//   (b) Even if it compiled, the schema assertions below would fail because
//       `template`, `hcp_org`, and `hcp_project` are still present in the schema
//       and `dynamic_outputs` does not yet exist.
//
// Goes GREEN when Kou completes E5 Task 2.
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

// TestReservationSchema_DynamicOutputsExists asserts that the `dynamic_outputs`
// attribute exists and is typed as a map of strings.
//
// RED: `dynamic_outputs` does not exist in the current schema.
func TestReservationSchema_DynamicOutputsExists(t *testing.T) {
	t.Parallel()

	s := resourceSchemaForDelete(t)

	attr, ok := s.Attributes["dynamic_outputs"]
	if !ok {
		t.Fatal("FAIL: attribute \"dynamic_outputs\" is absent from schema; must be a MapAttribute(StringType)")
	}

	// It must be a MapAttribute — the underlying type must expose a map element type.
	mapAttr, ok := attr.(rschema.MapAttribute)
	if !ok {
		t.Fatalf("FAIL: \"dynamic_outputs\" is %T, want rschema.MapAttribute", attr)
	}

	// Element type must be StringType.
	if mapAttr.ElementType != types.StringType {
		t.Errorf("FAIL: dynamic_outputs.ElementType = %T (%v), want types.StringType",
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
// includes dynamic_outputs (the new field) and serializes it into a tfsdk.State,
// then deserializes it back, asserting that the map is preserved.
//
// RED: reservationModel does not yet have a DynamicOutputs field, so the struct
// literal below will not compile.  This is the intentional compile-fail RED signal.
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
		t.Fatalf("building dynamic_outputs map: %v", diags)
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
	// DynamicOutputs field present.
	//
	// COMPILE-FAIL RED: reservationModel currently has Template/HCPOrg/HCPProject
	// and does NOT have DynamicOutputs. This literal will not compile until Kou
	// updates the struct in E5 Task 2.
	m := reservationModel{
		DynamicOutputs:          dynMap,
		Region:                  types.StringValue("us-east-2"),
		ReservationName:         types.StringValue("test-reservation"),
		Purpose:                 types.StringValue("Demo"),
		CollectionID:            types.StringValue("test-collection-id"),
		UserEmail:               types.StringValue("test@example.com"),
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

	// Assert dynamic_outputs round-tripped correctly.
	if got.DynamicOutputs.IsNull() || got.DynamicOutputs.IsUnknown() {
		t.Fatal("FAIL: dynamic_outputs is null/unknown after round-trip")
	}

	var gotElems map[string]types.String
	if diags := got.DynamicOutputs.ElementsAs(ctx, &gotElems, false); diags.HasError() {
		t.Fatalf("ElementsAs failed: %v", diags)
	}

	wantElems := map[string]string{
		"_04_hcp_org":     "org-test",
		"_05_hcp_project": "proj-test",
	}
	for k, want := range wantElems {
		gotV, ok := gotElems[k]
		if !ok {
			t.Errorf("FAIL: dynamic_outputs[%q] absent after round-trip", k)
			continue
		}
		if gotV.ValueString() != want {
			t.Errorf("FAIL: dynamic_outputs[%q] = %q, want %q", k, gotV.ValueString(), want)
		}
	}
	if len(gotElems) != len(wantElems) {
		t.Errorf("FAIL: dynamic_outputs has %d entries after round-trip, want %d",
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
//   - DynamicOutputs map(string) with two DDR entries.
//
// RED: will not compile until Kou adds DynamicOutputs to reservationModel and
// removes Template/HCPOrg/HCPProject.
//
// The existing TestDeleteUnit_* tests remain valid — they call buildDeleteState
// which will be updated in-place to match the new model in the same commit.
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
		t.Fatalf("building dynamic_outputs map: %v", diags)
	}

	// New-contract model: no Template/HCPOrg/HCPProject; DynamicOutputs present.
	// COMPILE-FAIL RED until Kou updates reservationModel.
	m := reservationModel{
		DynamicOutputs:          dynMap,
		Region:                  types.StringValue("us-east-2"),
		ReservationName:         types.StringValue("Reservation Name"),
		Purpose:                 types.StringValue("Demo"),
		CollectionID:            types.StringValue("test-collection-id"),
		UserEmail:               types.StringValue("test@example.com"),
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
