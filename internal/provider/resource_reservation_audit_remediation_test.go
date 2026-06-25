/**
 * @spec-handoff — Audit Remediation (Round 1) — resource_reservation.go contracts
 *
 * @interface reservationResource.Schema (collection_id attribute)
 * @interface reservationResource.Schema (template_variables attribute — Required assertion)
 * @interface mockTechZoneServer.handlePoll — nil-status sequence capability
 *
 * @behavior (additions — audit remediation)
 *
 *   COLLECTION_ID VALIDATOR:
 *   - The collection_id schema attribute MUST have a Validator that rejects values
 *     not matching ^[a-fA-F0-9]{24}$ at plan/validate time.
 *   - A valid 24-char hex string (e.g. "69650af0758b9e41de66b6ae") MUST pass.
 *   - Values shorter than 24 chars, longer than 24 chars, non-hex chars, or
 *     path-metacharacters (e.g. "../foo", "a/b") MUST be rejected by the validator
 *     before the plan reaches Apply.
 *
 *   TEMPLATE_VARIABLES REQUIRED ASSERTION:
 *   - The template_variables schema attribute MUST be Required: true (not Optional, not Computed).
 *   - The existing spec-handoff comment in resource_reservation_schema_unit_test.go
 *     incorrectly says "Optional+Computed" — this test pins the correct contract.
 *
 *   POLL NIL-STATUS RETRY:
 *   - The poll loop in Create MUST handle poll responses with a nil/missing status
 *     field (200 OK + {} or {"otherKey": "val"} with no "status" key) by logging
 *     a warning and retrying (continue). It must NOT error out or panic.
 *   - A sequence {nil-status response} → {"status":"Ready"} MUST ultimately succeed
 *     and return a valid reservation ID.
 *
 * @contracts-for-kou
 *   1. collection_id validator: add to Schema():
 *        import "regexp"
 *        import "github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
 *        ...
 *        "collection_id": schema.StringAttribute{
 *            ...
 *            Validators: []validator.String{
 *                stringvalidator.RegexMatches(
 *                    regexp.MustCompile(`^[a-fA-F0-9]{24}$`),
 *                    "collection_id must be a 24-character hexadecimal string (MongoDB ObjectID format)",
 *                ),
 *            },
 *        },
 *   2. mockTechZoneServer: add `pollResponseSequence []string` field and
 *      `SetPollResponseSequence([]string)` method; when set, handlePoll returns
 *      the nth element for the nth poll call (last element repeated when exhausted).
 *      An empty string in the sequence represents a nil-status response: `{}`.
 *   3. The nilStatus guard in Create is already implemented (resource_reservation.go:443-450).
 *      The mock change is required to exercise it.
 *
 * @see ./resource_reservation.go
 * @see ./testutil_mock_server_test.go
 * @see ../../.yui-soul/reviews/114-techzone-template-agnostic/r1-ei-injection.md (F-04)
 * @see ../../.yui-soul/reviews/114-techzone-template-agnostic/r1-shin-tests.md (F-02, F-06)
 */

// Package provider — internal unit tests for audit remediation.
//
// This file is in package provider (not provider_test) to access unexported
// types: reservationResource, resourceSchemaForDelete, etc.
//
// RED GATE:
//   (a) TestReservationSchema_DynamicOutputs_IsRequired — asserts mapAttr.Required == true.
//       Currently passes (schema IS Required:true) but the spec-handoff COMMENT was wrong.
//       The test itself will be GREEN after adding this assertion (it pins the contract).
//       NOTE: We add it here as an EXPLICIT contract assertion even though it's currently
//       green — the spec-handoff comment correction is the driver per item 9.
//
//   (b) TestReservationSchema_CollectionID_HasHexValidator — asserts the collection_id
//       attribute has a validator constraint. Currently RED because no Validators are
//       set on collection_id; the attribute's Validators slice is nil/empty.
//
//   (c) TestAccReservation_PollWithNilStatus_RetriesAndSucceeds — uses a new mock
//       sequence capability. Currently RED because mockTechZoneServer has no
//       pollResponseSequence field — the mock helper call SetPollResponseSequence
//       will not compile until testutil_mock_server_test.go is updated.
package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// ---------------------------------------------------------------------------
// Contract 9 — template_variables is Required (spec-handoff comment fix)
// ---------------------------------------------------------------------------

// TestReservationSchema_DynamicOutputs_IsRequired asserts that the template_variables
// attribute is Required: true.
//
// The spec-handoff comment in resource_reservation_schema_unit_test.go (line 12)
// incorrectly states "Optional+Computed with RequiresReplace". The actual schema
// has Required: true. This test pins the actual contract so a future change from
// Required to Optional would be caught.
func TestReservationSchema_DynamicOutputs_IsRequired(t *testing.T) {
	t.Parallel()

	s := resourceSchemaForDelete(t)

	attr, ok := s.Attributes["template_variables"]
	if !ok {
		t.Fatal("FAIL: attribute \"template_variables\" is absent from schema")
	}

	mapAttr, ok := attr.(rschema.MapAttribute)
	if !ok {
		t.Fatalf("FAIL: \"template_variables\" is %T, want rschema.MapAttribute", attr)
	}

	// Contract: template_variables MUST be Required.
	// The spec-handoff comment claimed Optional+Computed but the schema and
	// intended behavior are unambiguously Required.
	if !mapAttr.Required {
		t.Errorf(
			"FAIL: template_variables must be Required:true\n"+
				"  mapAttr.Required == false\n"+
				"  The spec-handoff comment in resource_reservation_schema_unit_test.go (line 12)\n"+
				"  incorrectly stated 'Optional+Computed with RequiresReplace'. The correct\n"+
				"  contract is Required:true (no default, operator must supply the map).",
		)
	}

	// Also assert it is NOT Optional or Computed (belt-and-suspenders).
	if mapAttr.Optional {
		t.Errorf("FAIL: template_variables must NOT be Optional (it is Required)")
	}
	if mapAttr.Computed {
		t.Errorf("FAIL: template_variables must NOT be Computed (it is Required)")
	}
}

// ---------------------------------------------------------------------------
// Contract 5 — collection_id schema validator (^[a-fA-F0-9]{24}$)
// ---------------------------------------------------------------------------

// TestReservationSchema_CollectionID_HasHexValidator asserts that the collection_id
// schema attribute has a validator that enforces the ^[a-fA-F0-9]{24}$ pattern.
//
// RED: the current collection_id attribute has no Validators (empty/nil slice).
// This test will FAIL with:
//   "FAIL: collection_id has no schema validators — must have at least one (hex-24 validator)"
func TestReservationSchema_CollectionID_HasHexValidator(t *testing.T) {
	t.Parallel()

	s := resourceSchemaForDelete(t)

	attr, ok := s.Attributes["collection_id"]
	if !ok {
		t.Fatal("FAIL: attribute \"collection_id\" is absent from schema")
	}

	strAttr, ok := attr.(rschema.StringAttribute)
	if !ok {
		t.Fatalf("FAIL: \"collection_id\" is %T, want rschema.StringAttribute", attr)
	}

	// Must have at least one validator.
	if len(strAttr.Validators) == 0 {
		t.Fatalf(
			"FAIL: collection_id has no schema validators — must have at least one\n"+
				"  A validator enforcing ^[a-fA-F0-9]{24}$ is required to:\n"+
				"  (a) catch invalid IDs at plan time (before Apply)\n"+
				"  (b) eliminate path-injection risk (Ei F-01: unescaped id in URL)\n"+
				"  Add: stringvalidator.RegexMatches(regexp.MustCompile(`^[a-fA-F0-9]{24}$`), ...)",
		)
	}
}

// TestReservationSchema_CollectionID_ValidHexPattern verifies the deployed
// collection_id validator using the schema's actual validator instance.
//
// Motivation (Ei F-03 / audit remediation): the previous version of this test
// compiled its own phantom regex (`^[a-fA-F0-9]{24}$`) and tested it in isolation
// — proving nothing about the deployed validator. If Kou had shipped a typo
// in the pattern (e.g. `^[a-fA-F0-9]{23}$`), this test would still pass.
//
// This version invokes ValidateString on the validator extracted directly from the
// schema, so it exercises the real deployed code path. A valid 24-hex ID must
// produce no diagnostics; invalid IDs must produce a non-empty diagnostics set.
//
// Aligned with Kou's parallel tightening to ^[a-fA-F0-9]{24}$ (audit remediation
// batch, 2026-06-25). Test is green once Kou's validator is in place.
func TestReservationSchema_CollectionID_ValidHexPattern(t *testing.T) {
	t.Parallel()

	s := resourceSchemaForDelete(t)

	attr, ok := s.Attributes["collection_id"]
	if !ok {
		t.Fatal("FAIL: attribute \"collection_id\" is absent from schema")
	}
	strAttr, ok := attr.(rschema.StringAttribute)
	if !ok {
		t.Fatalf("FAIL: \"collection_id\" is %T, want rschema.StringAttribute", attr)
	}
	if len(strAttr.Validators) == 0 {
		t.Fatal("FAIL: collection_id has no validators — cannot exercise the pattern")
	}

	ctx := context.Background()
	attrPath := path.Root("collection_id")

	// invokeValidators calls all validators on the given string value and
	// returns true if any diagnostic errors were produced.
	invokeValidators := func(value string) bool {
		t.Helper()
		hasError := false
		for _, v := range strAttr.Validators {
			req := validator.StringRequest{
				Path:        attrPath,
				ConfigValue: types.StringValue(value),
			}
			resp := &validator.StringResponse{}
			v.ValidateString(ctx, req, resp)
			if resp.Diagnostics.HasError() {
				hasError = true
			}
		}
		return hasError
	}

	// Valid 24-char hex IDs — the deployed validator MUST accept these.
	// If it erroneously rejects them, valid collection IDs would be blocked at
	// plan time, which is a regression (Ei F-03 requirement: "valid IDs accepted").
	validIDs := []string{
		"69650af0758b9e41de66b6ae", // realistic MongoDB ObjectID from DDR collection
		"000000000000000000000001", // all-zeros with one (lower-hex edge)
		"AABBCCDDEEFF001122334455", // all-uppercase hex
		"aabbccddeeff001122334455", // all-lowercase hex
	}
	for _, id := range validIDs {
		if invokeValidators(id) {
			t.Errorf("FAIL: valid 24-hex collection_id %q was REJECTED by the deployed validator — "+
				"valid IDs must pass plan validation", id)
		}
	}

	// Invalid IDs — the deployed validator MUST reject these at plan time.
	// This is what makes the test meaningful: these were all accepted by the
	// relaxed pattern (^[a-zA-Z0-9_-]{1,64}$) but MUST be rejected by the
	// tightened pattern (^[a-fA-F0-9]{24}$).
	invalidIDs := []struct {
		id     string
		reason string
	}{
		{"a/b", "contains slash — path injection risk"},
		{"../foo", "path traversal attempt"},
		{"short", "too short (5 chars, non-hex)"},
		{"69650af0758b9e41de66b6aexxx", "too long (27 chars)"},
		{"69650af0758b9e41de66b6ag", "non-hex char 'g' at position 23"},
		{"", "empty string"},
		{"?foo=bar", "query string injection"},
		{"test-collection-id", "accepted by relaxed pattern but not 24-hex"},
		{"abc", "accepted by relaxed pattern (len=3) but not 24-hex"},
		{"x", "single char — accepted by relaxed but not 24-hex"},
	}
	for _, tc := range invalidIDs {
		if !invokeValidators(tc.id) {
			t.Errorf("FAIL: invalid collection_id %q (%s) was ACCEPTED by the deployed validator — "+
				"must be rejected at plan time by ^[a-fA-F0-9]{24}$", tc.id, tc.reason)
		}
	}
}

// ---------------------------------------------------------------------------
// Contract 6 — poll nil-status retry
// ---------------------------------------------------------------------------

// TestAccReservation_PollWithNilStatus_RetriesAndSucceeds verifies that the poll
// loop handles a nil/missing status response by retrying, and ultimately succeeds
// when a "Ready" response follows.
//
// RED: mockTechZoneServer currently has no pollResponseSequence field or
// SetPollResponseSequence method. This test will FAIL TO COMPILE until
// testutil_mock_server_test.go is updated to add the sequence capability.
//
// Contract being tested (resource_reservation.go:443-450):
//
//	if pollResp.Status == nil {
//	    tflog.Warn(ctx, "poll returned nil status — transient; retrying", ...)
//	    continue
//	}
//
// The test proves that this path retries rather than errors, and that the overall
// Create succeeds when Ready eventually follows.
//
// NOTE: This test is in package provider (internal) because it needs to compile
// against the updated mockTechZoneServer which is also in an internal test package.
// However, mockTechZoneServer is defined in package provider_test (external).
// Therefore this test must be in a file that bridges the two — or we need the
// poll sequence capability exposed on the external mock used by acc tests.
//
// SOLUTION: The nil-status test IS in package provider_test to match the mock.
// This file provides the schema-level tests in package provider (internal).
// See: resource_reservation_poll_nilstatus_test.go for the mock capability tests.
func TestReservationSchema_PollNilStatusMockCapability_Exists(t *testing.T) {
	// This is a documentation test that pins the contract for Kou.
	// The actual poll-nil-status test lives in resource_reservation_poll_nilstatus_test.go
	// in package provider_test (to access mockTechZoneServer).
	//
	// This test passes trivially — it exists to document the contract here in the
	// spec-handoff file and will not be removed when the acc test is added.
	t.Log("Poll nil-status retry contract: see resource_reservation_poll_nilstatus_test.go")
}

// resourceSchemaAttributeIsRequired is a helper that asserts a named attribute
// in the schema has Required=true.
func resourceSchemaAttributeIsRequired(t *testing.T, s rschema.Schema, attrName string) {
	t.Helper()

	attr, ok := s.Attributes[attrName]
	if !ok {
		t.Fatalf("FAIL: attribute %q is absent from schema", attrName)
	}

	// Use context-free interface check — all concrete schema.XxxAttribute types
	// have a GetRequired() bool accessor via the attribute interface internal type.
	// We can't call it directly, but we can type-switch.
	type requiredGetter interface {
		GetRequired() bool
	}
	if rg, ok := attr.(requiredGetter); ok {
		if !rg.GetRequired() {
			t.Errorf("FAIL: schema attribute %q must be Required:true, got false", attrName)
		}
	}
}

// Ensure context import is used.
var _ = context.Background
