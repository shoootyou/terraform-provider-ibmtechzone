// Package provider — internal (white-box) unit tests for Update().
//
// These tests are in package provider (not provider_test) so they can access
// the unexported reservationResource and reservationModel types and call
// r.Update() directly — same convention as
// resource_reservation_delete_unit_test.go's Delete()-integration tests and
// resource_reservation_extension_test.go's Read()-integration tests.
package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
)

// ---------------------------------------------------------------------------
// r3 audit finding 5 (round 3, MEDIUM) — Update()'s copy of
// plan.ExtensionWindowFraction into state (resource_reservation.go, plan
// README Decision D9: explicitly flagged "load-bearing" — required only
// because Q1=B added a new operational, non-RequiresReplace attribute) had
// zero test coverage anywhere in the repo. Deleting that one line entirely
// leaves the full existing suite green (confirmed via mutation-and-revert
// below) — a future refactor silently dropping or reverting it would go
// completely undetected by CI.
// ---------------------------------------------------------------------------

// TestUpdateUnit_ExtensionWindowFraction_CopiedFromPlan: builds a state with
// one extension_window_fraction value and a plan with a different one, calls
// Update() directly, and asserts the resulting state reflects the PLAN's
// value — not the stale state value. Mirrors this package's established
// white-box direct-call convention (e.g.
// resource_reservation_extension_test.go's TestReadUnit_Extension_Succeeds_UpdatesState),
// applied to Update() instead of Read().
func TestUpdateUnit_ExtensionWindowFraction_CopiedFromPlan(t *testing.T) {
	t.Parallel()
	const reservationID = "update-res-1"

	// Update() makes zero HTTP calls and never reads r.pd/r.now (confirmed by
	// direct inspection of the function body) — a zero-value resource is
	// sufficient, no client or injected clock needed.
	r := &reservationResource{}
	s := resourceSchemaForDelete(t)

	const stateFraction = 0.5 // the state's PREVIOUS value (happens to be the schema default)
	const planFraction = 0.25 // the plan's NEW, different value

	// buildExtensionTestState (resource_reservation_extension_test.go) builds
	// a fully-populated reservationModel via a real tfsdk.State — reused here
	// for both state and plan (tfsdk.Plan and tfsdk.State share the identical
	// underlying {Raw tftypes.Value, Schema fwschema.Schema} shape, exactly
	// the same repackaging convention resource_reservation_create_redaction_test.go's
	// buildCreateTestPlan already relies on).
	state := buildExtensionTestState(t, s, reservationID, extensionTestDurationDays, stateFraction, 0, provisionUntilInsideWindow)
	planRaw := buildExtensionTestState(t, s, reservationID, extensionTestDurationDays, planFraction, 0, provisionUntilInsideWindow)
	plan := tfsdk.Plan{Schema: s, Raw: planRaw.Raw}

	req := resource.UpdateRequest{Plan: plan, State: state}
	resp := resource.UpdateResponse{State: state}

	r.Update(context.Background(), req, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Update(): expected no error, got: %v", resp.Diagnostics)
	}

	var got reservationModel
	if diags := resp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("resp.State.Get(): %v", diags)
	}

	if got.ExtensionWindowFraction.ValueFloat64() != planFraction {
		t.Errorf("Update(): state.ExtensionWindowFraction = %v, want %v (the PLAN's value — r3 audit "+
			"finding 5, D9's \"load-bearing\" wiring) — got the stale state value %v instead, which "+
			"would mean an edit to this attribute is silently discarded on Update()",
			got.ExtensionWindowFraction.ValueFloat64(), planFraction, stateFraction)
	}
}
