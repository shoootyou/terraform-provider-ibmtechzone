/**
 * @spec-handoff
 *
 * @interface buildExtensionPayload(userEmail, reservationID, extensionDate string) ([]byte, error)
 * @behavior
 *   - Returns JSON with exactly 5 keys: IBMID=userEmail, requestType="aws" (constant),
 *     extensionDate=extensionDate (verbatim, an ABSOLUTE ISO date, not a delta),
 *     reservationId=reservationID, id=reservationID (duplicate of reservationId —
 *     confirmed required by the real API alongside reservationId).
 *
 * @interface classifyExtensionResponse(status int, body []byte) extensionOutcome
 * @behavior
 *   - status in [200,300) → extensionSucceeded (body carries no usable data).
 *   - status==400 AND body decodes with policy.isExtendable != nil AND
 *     *policy.isExtendable==false → extensionNotPossible. This is the UNIVERSAL
 *     signal across both real rejection patterns (errors[] contents and the
 *     "error" string vary by sub-case and MUST be ignored).
 *   - Anything else (malformed JSON, policy.isExtendable absent/null, isExtendable
 *     true at 400, any non-{2xx,400} status incl. 300/500 regardless of body) →
 *     extensionAmbiguous.
 *
 * @interface extensionWindowFractionValidator (validator.Float64)
 * @behavior
 *   - ConfigValue null/unknown (attribute not set by user) → skip, zero diagnostics.
 *   - Reads sibling reservation_duration_days via req.Config.GetAttribute (RAW
 *     config, pre-default-resolution). If that sibling is null/unknown (KNOWN,
 *     ACCEPTED LIMITATION — see spec's "Known limitation" section) → skip, zero
 *     diagnostics, even if the fraction itself is out of spec.
 *   - days<=0 → skip (not this validator's job to validate the sibling itself).
 *   - fraction*days > days (i.e. fraction > 1.0) → AddAttributeError with the
 *     exact pinned message (see spec §3d table); fraction*days <= days → zero
 *     diagnostics (0.0 and 1.0 are both valid boundaries).
 *
 * @interface reservationResource.Read — extension-window integration point
 * @behavior
 *   - Inserted after IsTerminalStatus, before PastExpiry (spec §5).
 *   - ok==false ("cannot determine") → tflog.Warn, extension skipped, falls
 *     through unchanged to PastExpiry.
 *   - eligible==true + extensionSucceeded → state updated (end_date = the new
 *     computed extensionDate, extend_count = apiResp.ExtendCount+1), early
 *     return — PastExpiry is skipped entirely for this Read() call.
 *   - eligible==true + extensionNotPossible|extensionAmbiguous → falls through
 *     unchanged to PastExpiry (existing prune/recreate flow) — NEVER a blocking
 *     Diagnostics error for any extension-attempt failure mode.
 *   - eligible==false (routine, most common case) → no POST call at all.
 *
 * @edge-cases
 *   - Both 400 rejection patterns (limit-exhausted-after-N-uses vs.
 *     extensionLimit:0-from-creation) MUST collapse to the identical
 *     extensionNotPossible outcome — proves the parser ignores errors[]/error text.
 *   - At most one extension POST per Read() call; no in-process retry loop.
 *
 * @see ./resource_reservation.go (Kou implements in E6)
 * @see ../techzone/extension_window.go, ../techzone/extension_window_test.go
 * @see .yui-soul/plans/wip/04-expiry-fix-and-extension-window/e4-extension-window-spec.md §3/§5/§6
 * @see .yui-soul/ideas/terraform-provider-ibmtechzone.md (6 empirical sessions — wire contract source)
 */

// Package provider — internal (white-box) tests for the extension-window
// feature: payload builder, 400-response classifier, the
// extension_window_fraction cross-attribute validator, and the Read()
// integration point. In package provider (not provider_test) so tests can
// reach the unexported buildExtensionPayload, classifyExtensionResponse,
// extensionOutcome/extensionWindowFractionValidator types, and call
// r.Read() directly — same convention as
// resource_reservation_delete_unit_test.go's Delete()-integration tests.
//
// RED GATE (none of this exists yet — only Phase A's expiry.go fix has
// shipped): every test below either fails to COMPILE (buildExtensionPayload,
// classifyExtensionResponse, extensionOutcome/extensionSucceeded/
// extensionNotPossible/extensionAmbiguous, extensionWindowFractionValidator,
// tzExtensionRejection, reservationModel.ExtensionWindowFraction,
// reservationModel.ExtendCount, tzReservationResponse.ExtendCount, and
// derefInt64 are all undefined identifiers) or, if the file were somehow
// coerced to compile, would fail its assertions — because none of these
// symbols exist in resource_reservation.go yet. This mirrors the precedented
// compile-red pattern already used in this package by
// resource_reservation_schema_unit_test.go (Plan 114).
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/defaults"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-log/tflogtest"
	"github.com/shoootyou-ext/terraform-provider-ibmtechzone/internal/techzone"
)

// ---------------------------------------------------------------------------
// Fixtures — verbatim/reconstructed from the spec (§ "Test fixtures for Shin")
// and the 6-session idea file. Do not alter these bytes; they are pinned.
// ---------------------------------------------------------------------------

// extensionSuccessBody: verbatim, idea sessions 3/6. Carries no usable
// reservation data — the caller derives new state from the request it just sent.
const extensionSuccessBody = `{"message":"ok","status":200}`

// extensionRejectionLimitExhausted: verbatim, real DDR collection, idea
// session 6. errors:[] + validation.extension:true — the "used up the N
// allotted extensions" pattern.
const extensionRejectionLimitExhausted = `{
    "error": "Request out of policy scope.",
    "errors": [],
    "policy": {
        "name": "Third-Party-Client-Facing",
        "extensionLimit": 5,
        "extensionLength": 345600,
        "isExtendable": false,
        "extensionMaxDate": "2026-07-27T17:14:00.000Z",
        "extend": "2026-07-24T17:14:00.000Z",
        "inPolicy": true,
        "validation": { "extension": true, "opportunityProduct": true }
    }
}`

// extensionRejectionNeverExtendable: reconstructed from the documented table
// in idea session 4 (raw JSON wasn't captured there, only the table).
// errors:["Invalid extension date"] + validation.extension:false — the
// "extensionLimit:0 from creation" pattern. isExtendable:false is the only
// field the parser depends on, so exact fidelity of the other fields does not
// affect correctness.
const extensionRejectionNeverExtendable = `{
    "error": "...Invalid extension date.",
    "errors": ["Invalid extension date"],
    "policy": { "isExtendable": false, "extensionLimit": 0, "validation": { "extension": false } }
}`

// extensionAmbiguousNoPolicy: no evidence in the idea file for this shape —
// Taku's own construction (not a captured payload) to exercise the
// extensionAmbiguous branch when no policy object is present at all.
const extensionAmbiguousNoPolicy = `{"error":"some other error","errors":["Something else"]}`

// ---------------------------------------------------------------------------
// TestBuildExtensionPayload
// ---------------------------------------------------------------------------

func TestBuildExtensionPayload(t *testing.T) {
	t.Parallel()

	const userEmail = "user@example.com"
	const reservationID = "res-123"
	const extensionDate = "2026-07-24T00:00:00.000Z"

	raw, err := buildExtensionPayload(userEmail, reservationID, extensionDate)
	if err != nil {
		t.Fatalf("buildExtensionPayload returned error: %v", err)
	}

	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("buildExtensionPayload output is not valid JSON: %v (body: %s)", err, raw)
	}

	want := map[string]string{
		"IBMID":         userEmail,
		"requestType":   "aws",
		"extensionDate": extensionDate,
		"reservationId": reservationID,
		"id":            reservationID,
	}

	for k, wantV := range want {
		gotV, ok := got[k]
		if !ok {
			t.Errorf("buildExtensionPayload: key %q absent from payload", k)
			continue
		}
		if gotV != wantV {
			t.Errorf("buildExtensionPayload: %q = %q, want %q", k, gotV, wantV)
		}
	}
	if len(got) != len(want) {
		t.Errorf("buildExtensionPayload: payload has %d keys (%v), want exactly %d", len(got), got, len(want))
	}

	// id MUST duplicate reservationId — confirmed required by the real API
	// alongside reservationId (spec §3b), not merely "happens to be the same
	// value because both come from the same input variable."
	if got["id"] != got["reservationId"] {
		t.Errorf("buildExtensionPayload: id (%q) must equal reservationId (%q)", got["id"], got["reservationId"])
	}
}

// ---------------------------------------------------------------------------
// TestClassifyExtensionResponse
// ---------------------------------------------------------------------------

func TestClassifyExtensionResponse(t *testing.T) {
	t.Parallel()

	type row struct {
		name   string
		status int
		body   string
		want   extensionOutcome
	}

	rows := []row{
		{name: "success_200", status: 200, body: extensionSuccessBody, want: extensionSucceeded},
		{name: "success_299_upper_boundary", status: 299, body: extensionSuccessBody, want: extensionSucceeded},
		{name: "not_2xx_300_boundary_is_ambiguous_not_success", status: 300, body: extensionSuccessBody, want: extensionAmbiguous},

		// --- audit round-1 finding #5: a 2xx status is no longer trusted on
		// its own — the body must match the confirmed success shape. Ei
		// empirically confirmed that, pre-fix, an HTML body, an empty body,
		// or an unrelated JSON shape all resolved to extensionSucceeded
		// merely because the HTTP status happened to be 2xx. ---
		{name: "success_2xx_empty_body_is_ambiguous_not_success", status: 200, body: ``, want: extensionAmbiguous},
		{name: "success_2xx_html_body_is_ambiguous_not_success", status: 200, body: `<html><body>OK</body></html>`, want: extensionAmbiguous},
		{name: "success_2xx_unrelated_json_shape_is_ambiguous_not_success", status: 200, body: `{"foo":"bar"}`, want: extensionAmbiguous},
		{
			// r3 audit finding 4 (MEDIUM): the check requires BOTH markers
			// now (Status==200 AND Message=="ok"), not "either alone" — both
			// explicitly present-but-zero/empty must NOT count as a match.
			name: "success_2xx_status_zero_and_message_empty_is_ambiguous", status: 200,
			body: `{"status":0,"message":""}`, want: extensionAmbiguous,
		},
		{
			// r3 audit finding 4 (MEDIUM) — hardened from the pre-fix OR
			// check ("either marker alone sufficed", which this row's name
			// used to describe and assert as extensionSucceeded). Real
			// evidence (.yui-soul/ideas/terraform-provider-ibmtechzone.md,
			// sessions 3/6) shows the API always returns both fields
			// together in 100% of 10 observed successful calls — a message
			// with no status field at all no longer represents a real
			// observed shape, so it must now fall through to
			// extensionAmbiguous rather than being trusted alone.
			name: "success_2xx_message_only_no_status_field_now_ambiguous_after_hardening", status: 200,
			body: `{"message":"ok"}`, want: extensionAmbiguous,
		},
		{
			// r3 audit finding 4 (MEDIUM) — same hardening, the other
			// formerly-sufficient marker alone: status==200 with no message
			// field no longer represents a real observed shape either.
			name: "success_2xx_status_only_no_message_field_now_ambiguous_after_hardening", status: 200,
			body: `{"status":200}`, want: extensionAmbiguous,
		},
		{
			// r3 audit finding 4 (MEDIUM) — the core regression this
			// hardening fixes: under the OLD "either marker" OR check, this
			// internally-contradictory body (a message claiming success
			// alongside a status field explicitly claiming a server error)
			// incorrectly classified as extensionSucceeded purely because
			// Message was non-empty. The new check requires Status==200 AND
			// Message=="ok" together — status 500 fails the Status==200
			// half, so this must now be extensionAmbiguous.
			name:   "success_2xx_contradictory_status500_message_ok_is_now_ambiguous_after_hardening",
			status: 200,
			body:   `{"status":500,"message":"ok"}`,
			want:   extensionAmbiguous,
		},
		{
			// r3 audit finding 4 (MEDIUM) — a non-"ok" message value must
			// not pass, even paired with Status==200: the hardened check
			// compares Message against the exact real-observed value "ok",
			// not merely non-empty.
			name:   "success_2xx_status200_message_not_exactly_ok_is_ambiguous",
			status: 200,
			body:   `{"status":200,"message":"OK"}`,
			want:   extensionAmbiguous,
		},

		// --- both documented 400 rejection patterns → identical outcome ---
		{
			name:   "rejection_limit_exhausted_after_N_uses",
			status: 400,
			body:   extensionRejectionLimitExhausted,
			want:   extensionNotPossible,
		},
		{
			name:   "rejection_never_extendable_extensionLimit_0_from_creation",
			status: 400,
			body:   extensionRejectionNeverExtendable,
			want:   extensionNotPossible,
		},

		// --- ambiguous: unrecognized shapes / unexpected statuses ---
		{name: "ambiguous_no_policy_object", status: 400, body: extensionAmbiguousNoPolicy, want: extensionAmbiguous},
		{name: "ambiguous_malformed_json", status: 400, body: `{not valid json`, want: extensionAmbiguous},
		{name: "ambiguous_empty_body", status: 400, body: ``, want: extensionAmbiguous},
		{name: "ambiguous_policy_isExtendable_null", status: 400, body: `{"policy":{"isExtendable":null}}`, want: extensionAmbiguous},
		{
			// Boundary/regression coverage (derived directly from the given
			// `!= nil && !*IsExtendable` logic, not a captured real payload):
			// isExtendable:true at 400 must NOT be treated as extensionNotPossible.
			// Catches a `IsExtendable != nil` (missing negation) regression.
			name:   "ambiguous_policy_isExtendable_true_is_not_notPossible",
			status: 400,
			body:   `{"policy":{"isExtendable":true}}`,
			want:   extensionAmbiguous,
		},
		{
			// Any non-{2xx,400} status is ambiguous regardless of body — even a
			// body that WOULD parse as a valid rejection at 400.
			name:   "ambiguous_500_regardless_of_wouldbe_valid_400_body",
			status: 500,
			body:   extensionRejectionLimitExhausted,
			want:   extensionAmbiguous,
		},
	}

	for _, tc := range rows {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyExtensionResponse(tc.status, []byte(tc.body))
			if got != tc.want {
				t.Errorf("classifyExtensionResponse(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}

	// The zero value of extensionOutcome (extensionUnknown) must never be
	// returned by the function — every branch above resolves to one of the
	// three named outcomes.
	t.Run("never_returns_zero_value_extensionUnknown", func(t *testing.T) {
		t.Parallel()
		for _, tc := range rows {
			if classifyExtensionResponse(tc.status, []byte(tc.body)) == extensionUnknown {
				t.Errorf("classifyExtensionResponse(%d, %q) returned the zero-value extensionUnknown — this is always a bug",
					tc.status, tc.body)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// TestExtensionWindowFractionValidator
//
// Fixture-construction recipe taken directly from the spec's "Proven
// test-construction recipe" (already compiled and run by Taku against the
// real pinned terraform-plugin-framework@v1.19.0 in that session, and
// independently re-verified again by Shin in this session against a
// throwaway stand-in implementation before writing this file). Uses a
// minimal 2-attribute local schema rather than the full
// reservationResource{}.Schema() — the validator only interacts with these
// two attributes, and building a full tftypes Object for the entire real
// schema (nested requester_context, service_links list, template_variables
// map, ...) would add construction complexity with no bearing on the
// behavior under test. The spec explicitly permits this adaptation.
// ---------------------------------------------------------------------------

// extensionValidatorFixtureSchema is the minimal schema used to construct
// validator.Float64Request fixtures: only the two attributes the validator
// actually reads.
func extensionValidatorFixtureSchema() rschema.Schema {
	return rschema.Schema{
		Attributes: map[string]rschema.Attribute{
			"reservation_duration_days": rschema.Int64Attribute{Optional: true, Computed: true},
			"extension_window_fraction": rschema.Float64Attribute{Optional: true, Computed: true},
		},
	}
}

// newExtensionWindowFractionValidatorRequest builds a validator.Float64Request
// against extensionValidatorFixtureSchema(). fraction/durationDays == nil
// means "not set by the user" (null in the raw config) — the trigger for the
// accepted known-limitation skip when durationDays is nil.
func newExtensionWindowFractionValidatorRequest(t *testing.T, fraction *float64, durationDays *int64) validator.Float64Request {
	t.Helper()
	ctx := context.Background()
	s := extensionValidatorFixtureSchema()

	durationVal := tftypes.NewValue(tftypes.Number, nil)
	if durationDays != nil {
		durationVal = tftypes.NewValue(tftypes.Number, *durationDays)
	}

	var fractionConfigValue types.Float64
	fractionVal := tftypes.NewValue(tftypes.Number, nil)
	if fraction != nil {
		fractionVal = tftypes.NewValue(tftypes.Number, *fraction)
		fractionConfigValue = types.Float64Value(*fraction)
	} else {
		fractionConfigValue = types.Float64Null()
	}

	// tftypes.Object requires every declared attribute key present in the
	// value map, null or not — omitting a key panics ("required attribute
	// ... not set"). Confirmed by testing (spec's known-limitation section).
	raw := tftypes.NewValue(s.Type().TerraformType(ctx), map[string]tftypes.Value{
		"reservation_duration_days": durationVal,
		"extension_window_fraction": fractionVal,
	})

	return validator.Float64Request{
		Path:        path.Root("extension_window_fraction"),
		Config:      tfsdk.Config{Schema: s, Raw: raw},
		ConfigValue: fractionConfigValue,
	}
}

func f64ptr(v float64) *float64 { return &v }
func i64ptr(v int64) *int64     { return &v }

func TestExtensionWindowFractionValidator(t *testing.T) {
	t.Parallel()

	type row struct {
		name       string
		fraction   *float64 // nil == unset/null in raw config
		duration   *int64   // nil == unset/null in raw config
		wantErr    bool
		wantDetail string // exact Detail() text, checked only when wantErr
	}

	rows := []row{
		// --- 5 pinned rows, spec §3(d) table ---
		{
			name:     "row1_invalid_1.5_over_duration_4",
			fraction: f64ptr(1.5), duration: i64ptr(4),
			wantErr: true,
			wantDetail: "extension_window_fraction is 1.5, and reservation_duration_days is 4, so the " +
				"computed extension window would be 6 days — longer than the reservation's own 4-day " +
				"duration. extension_window_fraction must be in the range (0, 1] so that " +
				"extension_window_fraction × reservation_duration_days never exceeds reservation_duration_days.",
		},
		{
			name:     "row2_valid_inclusive_boundary_1.0_over_4",
			fraction: f64ptr(1.0), duration: i64ptr(4),
			wantErr: false,
		},
		{
			name:     "row3_valid_default_0.5_over_4",
			fraction: f64ptr(0.5), duration: i64ptr(4),
			wantErr: false,
		},
		{
			name:     "row4_valid_degenerate_zero_0.0_over_4",
			fraction: f64ptr(0.0), duration: i64ptr(4),
			wantErr: false,
		},
		{
			// KNOWN LIMITATION, accepted by design (spec's dedicated section):
			// reservation_duration_days unset (null in raw config, relying on
			// its own schema Default) → validator CANNOT know the resolved
			// value yet → skips validation entirely, even though 1.5 is out
			// of spec. This is the pinned, intentional accepted behavior —
			// NOT a bug to fix. A future refactor that silently starts
			// erroring here (or, worse, panicking) must surface as a
			// deliberate discussion, not an unnoticed regression.
			name:     "row5_known_limitation_1.5_with_duration_unset_is_VALID_by_design",
			fraction: f64ptr(1.5), duration: nil,
			wantErr: false,
		},

		// --- additional coverage beyond the pinned 5, derived directly from
		// the validator's own first two guard clauses (not invented server/
		// wire behavior — pure schema/validator logic already fully given in
		// the spec's code) ---
		{
			// The single most common real-world path: user never touches
			// extension_window_fraction at all, relying on its own Default.
			name:     "extra_fraction_itself_unset_skips_entirely",
			fraction: nil, duration: i64ptr(4),
			wantErr: false,
		},
		{
			// Nonsensical sibling value; validating reservation_duration_days
			// itself is not this validator's job (InExtensionWindow fails
			// closed on durationDays<=0 at runtime regardless, spec §1).
			name:     "extra_duration_zero_is_not_this_validators_job",
			fraction: f64ptr(1.5), duration: i64ptr(0),
			wantErr: false,
		},
		{
			name:     "extra_duration_negative_is_not_this_validators_job",
			fraction: f64ptr(1.5), duration: i64ptr(-5),
			wantErr: false,
		},
	}

	for _, tc := range rows {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := newExtensionWindowFractionValidatorRequest(t, tc.fraction, tc.duration)
			resp := &validator.Float64Response{}

			extensionWindowFractionValidator{}.ValidateFloat64(context.Background(), req, resp)

			gotErr := resp.Diagnostics.HasError()
			if gotErr != tc.wantErr {
				t.Fatalf("ValidateFloat64: HasError() = %v, want %v (diagnostics: %v)", gotErr, tc.wantErr, resp.Diagnostics)
			}
			if !tc.wantErr {
				return
			}
			if len(resp.Diagnostics) != 1 {
				t.Fatalf("ValidateFloat64: expected exactly 1 diagnostic, got %d: %v", len(resp.Diagnostics), resp.Diagnostics)
			}
			if gotDetail := resp.Diagnostics[0].Detail(); gotDetail != tc.wantDetail {
				t.Errorf("ValidateFloat64 error detail mismatch:\n  got:  %q\n  want: %q", gotDetail, tc.wantDetail)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestReservationSchema_ExtensionWindowFraction / TestReservationSchema_ExtendCount
//
// Direct schema-shape assertions against the REAL reservationResource{}.Schema()
// (via resourceSchemaForDelete(t), the same helper resource_reservation_delete_unit_test.go
// and resource_reservation_schema_unit_test.go already use) — distinct from
// TestExtensionWindowFractionValidator above, which exercises the validator
// type in isolation against a minimal local schema. These two tests pin that
// the production Schema() actually wires the new attributes in with the right
// shape (Optional/Computed/Default/validator attachment), matching this
// package's established convention for schema-shape tests (see
// TestReservationSchema_RequesterContextExists in
// resource_reservation_schema_unit_test.go).
// ---------------------------------------------------------------------------

func TestReservationSchema_ExtensionWindowFraction(t *testing.T) {
	t.Parallel()
	s := resourceSchemaForDelete(t)

	raw, ok := s.Attributes["extension_window_fraction"]
	if !ok {
		t.Fatal("FAIL: `extension_window_fraction` attribute is absent from schema; must be an " +
			"Optional+Computed Float64Attribute with a Default (spec §3d)")
	}
	fa, ok := raw.(rschema.Float64Attribute)
	if !ok {
		t.Fatalf("FAIL: `extension_window_fraction` is %T, want rschema.Float64Attribute", raw)
	}
	if !fa.Optional {
		t.Error("FAIL: `extension_window_fraction` must be Optional (user may override the default fraction)")
	}
	if !fa.Computed {
		t.Error("FAIL: `extension_window_fraction` must be Computed (so the Default resolves for configs that don't set it)")
	}
	if fa.Default == nil {
		t.Error("FAIL: `extension_window_fraction` must have a Default " +
			"(spec: float64default.StaticFloat64(techzone.DefaultExtensionWindowFraction))")
	} else {
		// Audit round-1 finding #6: the checks above only confirm a Default
		// is PRESENT, never that it resolves to the RIGHT value. A typo that
		// replaces techzone.DefaultExtensionWindowFraction with an incorrect
		// literal in Schema() would pass every assertion above undetected.
		// Resolve the default the same way the Framework itself would at
		// plan time and assert the actual resolved value.
		defResp := &defaults.Float64Response{}
		fa.Default.DefaultFloat64(context.Background(), defaults.Float64Request{Path: path.Root("extension_window_fraction")}, defResp)
		if defResp.Diagnostics.HasError() {
			t.Fatalf("FAIL: extension_window_fraction Default.DefaultFloat64() returned diagnostics: %v", defResp.Diagnostics)
		}
		if got := defResp.PlanValue.ValueFloat64(); got != techzone.DefaultExtensionWindowFraction {
			t.Errorf("FAIL: extension_window_fraction default value = %v, want %v (techzone.DefaultExtensionWindowFraction — "+
				"this assertion catches a typo'd literal that the presence-only check above would miss)",
				got, techzone.DefaultExtensionWindowFraction)
		}
	}
	if len(fa.Validators) == 0 {
		t.Fatal("FAIL: `extension_window_fraction` must have at least one Validator (extensionWindowFractionValidator)")
	}
	foundValidator := false
	for _, v := range fa.Validators {
		if _, ok := v.(extensionWindowFractionValidator); ok {
			foundValidator = true
			break
		}
	}
	if !foundValidator {
		t.Error("FAIL: `extension_window_fraction` Validators must include extensionWindowFractionValidator{}")
	}
}

func TestReservationSchema_ExtendCount(t *testing.T) {
	t.Parallel()
	s := resourceSchemaForDelete(t)

	raw, ok := s.Attributes["extend_count"]
	if !ok {
		t.Fatal("FAIL: `extend_count` attribute is absent from schema; must be a Computed Int64Attribute (spec §4)")
	}
	ia, ok := raw.(rschema.Int64Attribute)
	if !ok {
		t.Fatalf("FAIL: `extend_count` is %T, want rschema.Int64Attribute", raw)
	}
	if !ia.Computed {
		t.Error("FAIL: `extend_count` must be Computed (read-only, wire-sourced from extendCount)")
	}
	if ia.Optional || ia.Required {
		t.Error("FAIL: `extend_count` must be Computed-only — not user-settable (Optional and Required must both be false)")
	}
	if len(ia.PlanModifiers) == 0 {
		t.Error("FAIL: `extend_count` should carry a PlanModifier (int64planmodifier.UseStateForUnknown(), " +
			"matching status/start_date/end_date's existing audit-fix-H1 rationale: avoid " +
			"\"Provider produced inconsistent result\" on operational-only Updates)")
	}
}

// ---------------------------------------------------------------------------
// Read() integration — extension-window attempt
//
// White-box, direct r.Read(ctx, req, &resp) calls (no full acceptance-test
// harness / no TF_ACC needed), mirroring
// resource_reservation_delete_unit_test.go's existing convention for
// integration-testing a single resource method against a controllable
// in-process httptest.Server.
// ---------------------------------------------------------------------------

// extensionReadFixture is a minimal, purpose-built in-process mock serving
// exactly the two endpoints the Read()-integration tests below need:
//
//	GET  /api/reservation/aws/<id>  — canonical read (fixed response)
//	POST /api/reservation/aws/<id>  — extension attempt (fixed response)
//
// Deliberately NOT reusing testutil_mock_server_test.go's mockTechZoneServer:
// that shared fixture has no POST-with-id handler yet (only POST
// /api/reservation/aws without an id, for Create), and extending a fixture
// shared by ~10 other test files is a larger, riskier surface than a small
// self-contained server scoped to exactly this new file — consistent with
// buildReservationResource's own existing per-file httptest.Server pattern.
type extensionReadFixture struct {
	t *testing.T

	mu sync.Mutex

	getStatus int
	getBody   string

	postStatus int
	postBody   string

	// forcePostTransportError, when true, makes the POST handler hijack the
	// underlying TCP connection and close it without writing any HTTP
	// response — simulating a genuine transport-layer failure (connectivity
	// error, as opposed to a completed HTTP response with a non-2xx status).
	// Hallazgo 3 remediation (audit round 1): exercises the extErr != nil
	// branch in Read()'s extension-POST block, which had zero dedicated
	// test coverage before this fix.
	forcePostTransportError bool

	postCallCount int
	getCallCount  int
	lastPostBody  []byte
}

func newExtensionReadFixture(t *testing.T, getStatus int, getBody string, postStatus int, postBody string) *extensionReadFixture {
	t.Helper()
	return &extensionReadFixture{
		t: t, getStatus: getStatus, getBody: getBody, postStatus: postStatus, postBody: postBody,
	}
}

// newExtensionReadFixtureWithPostTransportError builds a fixture whose GET
// handler responds normally (getStatus/getBody) but whose POST handler
// forces a transport-layer failure (hijack + abrupt connection close, no
// HTTP response ever written) — see forcePostTransportError above.
func newExtensionReadFixtureWithPostTransportError(t *testing.T, getStatus int, getBody string) *extensionReadFixture {
	t.Helper()
	return &extensionReadFixture{
		t: t, getStatus: getStatus, getBody: getBody,
		forcePostTransportError: true,
	}
}

// Server starts the httptest.Server and registers cleanup.
func (f *extensionReadFixture) Server() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	f.t.Cleanup(srv.Close)
	return srv
}

func (f *extensionReadFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/reservation/aws/"):
		f.mu.Lock()
		f.getCallCount++
		status, body := f.getStatus, f.getBody
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)

	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/reservation/aws/"):
		f.mu.Lock()
		f.postCallCount++
		force := f.forcePostTransportError
		f.mu.Unlock()

		if force {
			// Hijack the raw TCP connection and close it without writing any
			// response — the client sees a genuine transport error (e.g.
			// "EOF" / "connection reset"), not a completed HTTP response.
			hj, ok := w.(http.Hijacker)
			if !ok {
				f.t.Fatal("extensionReadFixture: ResponseWriter does not support hijacking " +
					"(needed to simulate a POST transport error)")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				f.t.Fatalf("extensionReadFixture: hijack failed: %v", err)
				return
			}
			conn.Close()
			return
		}

		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.lastPostBody = raw
		status, body := f.postStatus, f.postBody
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)

	default:
		f.t.Logf("extensionReadFixture: unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func (f *extensionReadFixture) PostCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.postCallCount
}

func (f *extensionReadFixture) GetCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCallCount
}

func (f *extensionReadFixture) LastPostBody() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPostBody
}

// canonicalReadBody renders a GET /api/reservation/aws/<id> response body
// with the given status/provisionUntil/extendCount.
func canonicalReadBody(status, provisionUntil string, extendCount int64) string {
	return fmt.Sprintf(`{
		"id": "ext-res-1",
		"status": %q,
		"serviceLinks": [],
		"provisionDate": "2026-07-01T00:00:00Z",
		"provisionUntil": %q,
		"extendCount": %d
	}`, status, provisionUntil, extendCount)
}

// buildExtensionTestResource constructs a reservationResource wired to
// srvURL with an injected (fixed, non-time.Now) clock — required for
// deterministic window-eligibility assertions.
func buildExtensionTestResource(t *testing.T, srvURL string, nowFn func() time.Time) *reservationResource {
	t.Helper()
	client, err := techzone.NewClient(srvURL, sentinelTokenInternal)
	if err != nil {
		t.Fatalf("NewClient(%s): %v", srvURL, err)
	}
	return &reservationResource{
		pd:  &providerData{Client: client},
		now: nowFn,
	}
}

// buildExtensionTestState builds a tfsdk.State populated with a reservationModel
// carrying the given reservationID/durationDays/windowFraction/extendCount/endDate.
// Structurally mirrors buildDeleteState (same package, resource_reservation_delete_unit_test.go)
// with the two new extension-window fields added.
func buildExtensionTestState(t *testing.T, s rschema.Schema, reservationID string, durationDays int64, windowFraction float64, extendCount int64, endDate string) tfsdk.State {
	t.Helper()
	ctx := context.Background()

	rawType := s.Type().TerraformType(ctx)
	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(rawType, nil)}

	emptyLinks, diags := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: serviceLinkAttrTypes}, []ServiceLinkModel{})
	if diags.HasError() {
		t.Fatalf("building empty service_links: %v", diags)
	}

	dynMap, dynDiags := types.MapValue(types.StringType, map[string]attr.Value{
		"_04_hcp_org":     types.StringValue("test-hcp-org"),
		"_05_hcp_project": types.StringValue("test-hcp-project"),
	})
	if dynDiags.HasError() {
		t.Fatalf("building template_variables map: %v", dynDiags)
	}

	rcAttrTypes := map[string]attr.Type{
		"opportunity": types.ListType{ElemType: types.StringType},
		"iui":         types.StringType,
	}
	nullRC := types.ObjectNull(rcAttrTypes)

	m := reservationModel{
		TemplateVariables:       dynMap,
		Region:                  types.StringValue("us-east-2"),
		ReservationName:         types.StringValue("Reservation Name"),
		Purpose:                 types.StringValue("Demo"),
		CollectionID:            types.StringValue("test-collection-id"),
		UserEmail:               types.StringValue("test@example.com"),
		RequesterContext:        nullRC,
		ReservationDurationDays: types.Int64Value(durationDays),
		TimeoutMinutes:          types.Int64Value(30),
		ExtensionWindowFraction: types.Float64Value(windowFraction), // NEW field — spec §3d
		ID:                      types.StringValue(reservationID),
		Status:                  types.StringValue("Ready"),
		ServiceLinks:            emptyLinks,
		StartDate:               types.StringValue("2026-07-01T00:00:00Z"),
		EndDate:                 types.StringValue(endDate),
		ExtendCount:             types.Int64Value(extendCount), // NEW field — spec §4
	}

	if diags := state.Set(ctx, m); diags.HasError() {
		t.Fatalf("state.Set() failed: %v", diags)
	}
	return state
}

// captureTFLogWarnings runs fn with a tflog context wired to an in-memory
// buffer and returns every log entry with @level=="warn". Mirrors
// tflog_sentinel_test.go's existing tflogtest.RootLogger convention.
func captureTFLogWarnings(t *testing.T, fn func(ctx context.Context)) []map[string]interface{} {
	t.Helper()
	var buf bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &buf)

	fn(ctx)

	entries, err := tflogtest.MultilineJSONDecode(&buf)
	if err != nil {
		t.Logf("captureTFLogWarnings: MultilineJSONDecode: %v (may be empty log)", err)
		return nil
	}
	var warns []map[string]interface{}
	for _, e := range entries {
		if lvl, _ := e["@level"].(string); lvl == "warn" {
			warns = append(warns, e)
		}
	}
	return warns
}

// captureAllTFLogEntries runs fn with a tflog context wired to an in-memory
// buffer and returns every log entry regardless of level. Broader than
// captureTFLogWarnings above (which filters to @level=="warn" only) — used
// where a test needs to scan every field of every log entry at any level,
// mirroring tflog_sentinel_test.go's own convention of scanning ALL log
// levels for a token/body leak, not just warnings (Hallazgo 1/5 remediation,
// audit round 1).
func captureAllTFLogEntries(t *testing.T, fn func(ctx context.Context)) []map[string]interface{} {
	t.Helper()
	var buf bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &buf)

	fn(ctx)

	entries, err := tflogtest.MultilineJSONDecode(&buf)
	if err != nil {
		t.Logf("captureAllTFLogEntries: MultilineJSONDecode: %v (may be empty log)", err)
		return nil
	}
	return entries
}

// fixedNowForExtensionTests is the reference "now" for every Read()-integration
// scenario below: 2026-07-20T12:00:00Z. Independently verified via Go's own
// time package in this session (never hand-calculated, per plan README
// Decision D5): combined with provisionUntil = fixedNow-1h and
// durationDays=4/fraction=0.5, InExtensionWindow returns (true,true) AND
// PastExpiry (the pre-existing, unrelated check) ALSO returns true for the
// same provisionUntil — proving the reservation would have been PRUNED under
// the pre-extension-window code path, but a non-terminal status + successful
// extension intercepts it first (spec §5's exact insertion-point contract).
func fixedNowForExtensionTests() time.Time {
	return time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
}

const extensionTestDurationDays = int64(4)
const extensionTestWindowFraction = 0.5 // default

// provisionUntilInsideWindow: fixedNow - 1h. Independently verified (this
// session): InExtensionWindow(provisionUntilInsideWindow, fixedNow, 4, 0.5) ==
// (true, true); PastExpiry(provisionUntilInsideWindow, fixedNow) == true.
const provisionUntilInsideWindow = "2026-07-20T11:00:00Z"

// wantNextExtensionDate: NextExtensionDate(provisionUntilInsideWindow, 4).
// Independently verified via Go's time package (this session):
// time.Date(2026,7,20,11,0,0,0,UTC).Add(4*24h).Format("2006-01-02T15:04:05.000Z").
const wantNextExtensionDate = "2026-07-24T11:00:00.000Z"

// provisionUntilFarFuture: fixedNow + 365 days — well outside the window
// regardless of fraction, exercising the routine "not yet eligible" majority
// case (no extension attempt at all).
const provisionUntilFarFuture = "2027-07-20T12:00:00Z"

// TestReadUnit_Extension_Succeeds_UpdatesState: extension succeeds → state
// updated (end_date advances, extend_count increments), early return skips
// PastExpiry, exactly one POST sent with the correct payload shape.
func TestReadUnit_Extension_Succeeds_UpdatesState(t *testing.T) {
	t.Parallel()
	const reservationID = "ext-res-1"
	const initialExtendCount = int64(2)

	fx := newExtensionReadFixture(t,
		http.StatusOK, canonicalReadBody("Ready", provisionUntilInsideWindow, initialExtendCount),
		http.StatusOK, extensionSuccessBody,
	)
	srv := fx.Server()

	r := buildExtensionTestResource(t, srv.URL, fixedNowForExtensionTests)
	s := resourceSchemaForDelete(t)
	state := buildExtensionTestState(t, s, reservationID, extensionTestDurationDays, extensionTestWindowFraction, initialExtendCount, provisionUntilInsideWindow)

	req := resource.ReadRequest{State: state}
	resp := resource.ReadResponse{State: state}
	r.Read(context.Background(), req, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Read(): expected no error, got: %v", resp.Diagnostics)
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("Read(): resource was removed from state, want it preserved (extension succeeded)")
	}
	if got := fx.PostCallCount(); got != 1 {
		t.Errorf("Read(): extension POST called %d times, want exactly 1", got)
	}
	if got := fx.GetCallCount(); got != 1 {
		t.Errorf("Read(): canonical GET called %d times, want exactly 1 (Read() has no poll/retry loop, unlike Create())", got)
	}

	var got reservationModel
	if diags := resp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("resp.State.Get(): %v", diags)
	}
	if got.EndDate.ValueString() != wantNextExtensionDate {
		t.Errorf("end_date after successful extension = %q, want %q", got.EndDate.ValueString(), wantNextExtensionDate)
	}
	if want := initialExtendCount + 1; got.ExtendCount.ValueInt64() != want {
		t.Errorf("extend_count after successful extension = %d, want %d", got.ExtendCount.ValueInt64(), want)
	}
	if got.ID.ValueString() != reservationID {
		t.Errorf("id after successful extension = %q, want %q (must be preserved)", got.ID.ValueString(), reservationID)
	}

	// Cross-validate the actual wire body sent end-to-end (not just the pure
	// buildExtensionPayload unit test) — proves Read() wired the builder
	// correctly with the real computed extensionDate and state values.
	var sentPayload map[string]string
	if err := json.Unmarshal(fx.LastPostBody(), &sentPayload); err != nil {
		t.Fatalf("extension POST body is not valid JSON: %v (body: %s)", err, fx.LastPostBody())
	}
	if sentPayload["extensionDate"] != wantNextExtensionDate {
		t.Errorf("sent extensionDate = %q, want %q", sentPayload["extensionDate"], wantNextExtensionDate)
	}
	if sentPayload["reservationId"] != reservationID || sentPayload["id"] != reservationID {
		t.Errorf("sent reservationId/id = %q/%q, want both %q", sentPayload["reservationId"], sentPayload["id"], reservationID)
	}
	if sentPayload["IBMID"] != "test@example.com" {
		t.Errorf("sent IBMID = %q, want %q", sentPayload["IBMID"], "test@example.com")
	}
	if sentPayload["requestType"] != "aws" {
		t.Errorf("sent requestType = %q, want %q", sentPayload["requestType"], "aws")
	}
}

// runExtensionRejectionFallsThroughTest is the shared body for both 400
// rejection patterns: extension is attempted (eligible), rejected, and MUST
// fall through unchanged to the existing PastExpiry prune — WITHOUT ever
// raising a blocking Diagnostics error.
func runExtensionRejectionFallsThroughTest(t *testing.T, rejectionBody string) {
	t.Helper()
	const reservationID = "ext-res-1"
	const initialExtendCount = int64(5)

	fx := newExtensionReadFixture(t,
		http.StatusOK, canonicalReadBody("Ready", provisionUntilInsideWindow, initialExtendCount),
		http.StatusBadRequest, rejectionBody,
	)
	srv := fx.Server()

	r := buildExtensionTestResource(t, srv.URL, fixedNowForExtensionTests)
	s := resourceSchemaForDelete(t)
	state := buildExtensionTestState(t, s, reservationID, extensionTestDurationDays, extensionTestWindowFraction, initialExtendCount, provisionUntilInsideWindow)

	req := resource.ReadRequest{State: state}
	resp := resource.ReadResponse{State: state}
	r.Read(context.Background(), req, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Read(): a rejected extension attempt must NEVER raise a blocking error, got: %v", resp.Diagnostics)
	}
	if got := fx.PostCallCount(); got != 1 {
		t.Errorf("Read(): extension POST called %d times, want exactly 1", got)
	}
	// provisionUntilInsideWindow is strictly before fixedNow (verified this
	// session) — the existing, unchanged PastExpiry check must prune it once
	// the extension attempt falls through.
	if !resp.State.Raw.IsNull() {
		t.Error("Read(): rejected extension must fall through to the existing PastExpiry prune — resource should have been removed from state, but it was not")
	}
}

// TestReadUnit_Extension_RejectedLimitExhausted_FallsThroughToPrune: the
// "used up the allotted extensions" 400 pattern.
func TestReadUnit_Extension_RejectedLimitExhausted_FallsThroughToPrune(t *testing.T) {
	t.Parallel()
	runExtensionRejectionFallsThroughTest(t, extensionRejectionLimitExhausted)
}

// TestReadUnit_Extension_RejectedNeverExtendable_FallsThroughToPrune: the
// "extensionLimit:0 from creation" 400 pattern — proves BOTH documented
// rejection patterns collapse to the identical outcome via policy.isExtendable
// alone, ignoring errors[]/error text (spec §3c).
func TestReadUnit_Extension_RejectedNeverExtendable_FallsThroughToPrune(t *testing.T) {
	t.Parallel()
	runExtensionRejectionFallsThroughTest(t, extensionRejectionNeverExtendable)
}

// TestReadUnit_Extension_AmbiguousResponse_FallsThroughToPrune_LogsWarn:
// an unrecognized POST response (500, no usable body) must ALSO fall through
// without blocking — AND must be observable via tflog.Warn (the concrete fix
// for the fail-silent-to-KEEP pattern; an unrecognized shape must never be
// silently equivalent to a clean, confirmed rejection).
func TestReadUnit_Extension_AmbiguousResponse_FallsThroughToPrune_LogsWarn(t *testing.T) {
	t.Parallel()
	const reservationID = "ext-res-1"
	const initialExtendCount = int64(0)

	fx := newExtensionReadFixture(t,
		http.StatusOK, canonicalReadBody("Ready", provisionUntilInsideWindow, initialExtendCount),
		http.StatusInternalServerError, `{"error":"internal server error"}`,
	)
	srv := fx.Server()

	r := buildExtensionTestResource(t, srv.URL, fixedNowForExtensionTests)
	s := resourceSchemaForDelete(t)
	state := buildExtensionTestState(t, s, reservationID, extensionTestDurationDays, extensionTestWindowFraction, initialExtendCount, provisionUntilInsideWindow)

	req := resource.ReadRequest{State: state}
	resp := resource.ReadResponse{State: state}

	warns := captureTFLogWarnings(t, func(ctx context.Context) {
		r.Read(ctx, req, &resp)
	})

	if resp.Diagnostics.HasError() {
		t.Fatalf("Read(): an ambiguous extension response must NEVER raise a blocking error, got: %v", resp.Diagnostics)
	}
	if got := fx.PostCallCount(); got != 1 {
		t.Errorf("Read(): extension POST called %d times, want exactly 1", got)
	}
	if !resp.State.Raw.IsNull() {
		t.Error("Read(): ambiguous extension response must fall through to the existing PastExpiry prune, but resource was not removed")
	}
	if len(warns) == 0 {
		t.Error("Read(): an ambiguous/unrecognized extension response MUST be logged at tflog.Warn — " +
			"this is the concrete fix for the fail-silent-to-KEEP pattern (gotchas/ibm-techzone.md); " +
			"silence here reproduces exactly the class of bug this plan exists to close")
	}
}

// ---------------------------------------------------------------------------
// Hallazgo 1 (audit round 1, CRITICAL) — body_preview redaction for
// 401/403/302 on the extension POST's ambiguous branch.
//
// The rest of this file (poll loop, Final GET, Delete) already suppresses
// body_preview at 401/403 ("Ei R2 NF-01" — auth-rejection bodies may reflect
// credential material or SSO redirect HTML). The extensionAmbiguous branch
// in Read() did not apply that guard. These three tests prove the fix: a
// unique, detectable sentinel embedded in the extension-POST response body
// must never appear in ANY tflog entry when the POST completes with 401,
// 403, or 302 — mirroring tflog_sentinel_test.go's sentinel-scan convention
// for DoGet, applied here to the new Read()/extension-POST log path.
// ---------------------------------------------------------------------------

// extensionSentinelBodyValue is the unique marker embedded in a fabricated
// 401/403/302 extension-POST response body for the redaction tests below.
const extensionSentinelBodyValue = "SENTINEL-EXTENSION-BODY-DO-NOT-LOG"

// runExtensionAuthOrRedirectBodyRedactedTest is the shared body for the three
// Hallazgo-1 redaction tests: an extension POST completing with 401, 403, or
// 302 must never surface its response body in any log entry — only
// reservation_id + http_status are permitted.
func runExtensionAuthOrRedirectBodyRedactedTest(t *testing.T, postStatus int) {
	t.Helper()
	const reservationID = "ext-res-1"
	const initialExtendCount = int64(0)

	sentinelBody := fmt.Sprintf(`<html><body>Sign in to IBM — session %s</body></html>`, extensionSentinelBodyValue)

	fx := newExtensionReadFixture(t,
		http.StatusOK, canonicalReadBody("Ready", provisionUntilInsideWindow, initialExtendCount),
		postStatus, sentinelBody,
	)
	srv := fx.Server()

	r := buildExtensionTestResource(t, srv.URL, fixedNowForExtensionTests)
	s := resourceSchemaForDelete(t)
	state := buildExtensionTestState(t, s, reservationID, extensionTestDurationDays, extensionTestWindowFraction, initialExtendCount, provisionUntilInsideWindow)

	req := resource.ReadRequest{State: state}
	resp := resource.ReadResponse{State: state}

	entries := captureAllTFLogEntries(t, func(ctx context.Context) {
		r.Read(ctx, req, &resp)
	})

	if resp.Diagnostics.HasError() {
		t.Fatalf("Read(): extension POST status %d must never raise a blocking error, got: %v", postStatus, resp.Diagnostics)
	}
	if got := fx.PostCallCount(); got != 1 {
		t.Errorf("Read(): extension POST called %d times, want exactly 1", got)
	}
	if len(entries) == 0 {
		t.Fatalf("Read(): expected at least one log entry for extension POST status %d, got none", postStatus)
	}

	for i, e := range entries {
		for k, v := range e {
			if strings.Contains(fmt.Sprintf("%v", v), extensionSentinelBodyValue) {
				t.Errorf("Hallazgo 1: extension POST status %d — log entry %d field %q leaked the response body "+
					"sentinel (body_preview must be suppressed for 401/403/302, same guard as the poll loop / "+
					"Final GET / Delete paths): %v", postStatus, i, k, v)
			}
		}
	}
}

// TestReadUnit_Extension_POST401_BodyPreviewRedacted: a 401 on the extension
// POST (token invalidated in the window between the initial GET and this
// POST) must not leak its body into any log entry.
func TestReadUnit_Extension_POST401_BodyPreviewRedacted(t *testing.T) {
	t.Parallel()
	runExtensionAuthOrRedirectBodyRedactedTest(t, http.StatusUnauthorized)
}

// TestReadUnit_Extension_POST403_BodyPreviewRedacted: a 403 on the extension
// POST (extension endpoint requires an elevated role/scope the caller's
// token doesn't carry — tools.md documents this exact real-world case for
// the related /extend endpoint) must not leak its body.
func TestReadUnit_Extension_POST403_BodyPreviewRedacted(t *testing.T) {
	t.Parallel()
	runExtensionAuthOrRedirectBodyRedactedTest(t, http.StatusForbidden)
}

// TestReadUnit_Extension_POST302_BodyPreviewRedacted: the client never
// follows redirects (CheckRedirect returns http.ErrUseLastResponse), so a
// raw 302 can reach this branch directly and may carry SSO redirect HTML or
// session metadata; must not leak its body.
func TestReadUnit_Extension_POST302_BodyPreviewRedacted(t *testing.T) {
	t.Parallel()
	runExtensionAuthOrRedirectBodyRedactedTest(t, http.StatusFound)
}

// ---------------------------------------------------------------------------
// r2 finding 1 (audit round 2, HIGH) — the 401/403/302 body_preview
// redaction guard fixed in round 1 (above) for the extension-POST's
// succeeded/ambiguous branches was NOT retrofitted to 3 sibling call sites
// sharing the same risk pattern and endpoint base: Create()'s poll loop and
// Final GET (see resource_reservation_create_redaction_test.go), and
// Read()'s own initial GET (this test) — the most severe of the three,
// because a 302 there previously fell through to the generic non-2xx
// branch, which embeds the FULL raw body (up to 512 bytes) directly into
// resp.Diagnostics — a channel always visible to the user on every
// plan/apply/refresh, no TF_LOG required (unlike the tflog.Warn channel the
// other two call sites use).
// ---------------------------------------------------------------------------

// TestReadUnit_InitialGET302_BodyNotLeakedToDiagnostics: a 302 on Read()'s
// own initial canonical GET (BEFORE the extension-window attempt is ever
// reached) must route to the same auth-failure branch as 401/403 — a static
// canned message, no body content — instead of falling through to the
// generic non-2xx branch, which would leak the raw body (potential SSO
// redirect HTML or session metadata) into resp.Diagnostics.
func TestReadUnit_InitialGET302_BodyNotLeakedToDiagnostics(t *testing.T) {
	t.Parallel()
	const reservationID = "ext-res-1"

	sentinelBody := fmt.Sprintf(`<html><body>Sign in to IBM — session %s</body></html>`, extensionSentinelBodyValue)

	fx := newExtensionReadFixture(t,
		http.StatusFound, sentinelBody,
		http.StatusOK, extensionSuccessBody, // must never be reached — Read() returns on the initial GET's auth failure, before any extension attempt
	)
	srv := fx.Server()

	r := buildExtensionTestResource(t, srv.URL, fixedNowForExtensionTests)
	s := resourceSchemaForDelete(t)
	state := buildExtensionTestState(t, s, reservationID, extensionTestDurationDays, extensionTestWindowFraction, 0, provisionUntilInsideWindow)

	req := resource.ReadRequest{State: state}
	resp := resource.ReadResponse{State: state}

	entries := captureAllTFLogEntries(t, func(ctx context.Context) {
		r.Read(ctx, req, &resp)
	})

	if !resp.Diagnostics.HasError() {
		t.Fatal("Read(): a 302 on the initial GET must raise an auth-failure error, got none")
	}
	if got := fx.PostCallCount(); got != 0 {
		t.Errorf("Read(): extension POST called %d times, want exactly 0 — Read() must return "+
			"immediately on the initial GET's auth failure, before ever reaching the extension attempt", got)
	}

	// r2 finding 1's core assertion: the sentinel must not leak into
	// Diagnostics — the channel this finding is actually about. Before the
	// fix, httpStatus==302 fell through to the generic non-2xx branch, which
	// embeds truncate(body, 512) directly into AddError.
	var allMsgs strings.Builder
	for _, d := range resp.Diagnostics {
		allMsgs.WriteString(d.Summary())
		allMsgs.WriteString(" ")
		allMsgs.WriteString(d.Detail())
		allMsgs.WriteString(" ")
	}
	if combined := allMsgs.String(); strings.Contains(combined, extensionSentinelBodyValue) {
		t.Errorf("r2 finding 1: Read()'s initial-GET 302 leaked the response body sentinel into "+
			"Diagnostics — the 401/403/302 guard must route 302 to the canned auth-failure message, "+
			"never the generic non-2xx branch that embeds the raw body: %q", combined)
	}

	// Belt-and-suspenders: this path emits no tflog calls today (the
	// auth-failure branch uses only a static canned message), so entries
	// should be empty — scanning explicitly rather than assuming it, in case
	// that ever changes, mirroring this file's established sentinel-scan
	// convention.
	for i, e := range entries {
		for k, v := range e {
			if strings.Contains(fmt.Sprintf("%v", v), extensionSentinelBodyValue) {
				t.Errorf("r2 finding 1: log entry %d field %q leaked the initial-GET 302 body "+
					"sentinel: %v", i, k, v)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Hallazgo 5 (audit round 1, MEDIUM) — observability symmetry: the
// extensionSucceeded branch's "Reservation extended" log entry now includes
// body_preview (previously only new_provision_until).
//
// r2 finding 2 (audit round 2, MEDIUM): body_preview must be derived from the
// VALIDATED tzExtensionSuccessResponse struct (message, status), never raw
// body bytes — classifyExtensionResponse's shape check only requires
// Status==200 or a non-empty Message and does not reject unexpected
// additional fields (Go's json.Unmarshal ignores unknown keys), so logging
// truncate(extBody, 200) would reflect the body's full raw content verbatim,
// including any such unexpected field, exceeding what was actually
// validated. extensionSuccessBodyWithExtraField below carries one such
// unexpected field, embedding a sentinel, to prove it never leaks.
// ---------------------------------------------------------------------------

// extensionSuccessSentinelExtraField is embedded in an unexpected additional
// field of a fabricated 2xx extension-POST success body (r2 finding 2). It
// must never appear in any log entry — only the validated message/status
// fields may be reflected in body_preview.
const extensionSuccessSentinelExtraField = "SENTINEL-EXTRA-FIELD-DO-NOT-LOG"

// extensionSuccessBodyWithExtraField: the confirmed-real success shape
// ({"message":"ok","status":200}) plus one additional, unexpected field.
// classifyExtensionResponse still classifies this as extensionSucceeded
// (Go's json.Unmarshal ignores the unknown key) — the point of this fixture
// is to prove that the EXTRA field's content never reaches body_preview, not
// to test classification itself (TestClassifyExtensionResponse already
// covers that).
var extensionSuccessBodyWithExtraField = fmt.Sprintf(
	`{"message":"ok","status":200,"unexpected_field":%q}`, extensionSuccessSentinelExtraField)

func TestReadUnit_Extension_Succeeds_LogsBodyPreview(t *testing.T) {
	t.Parallel()
	const reservationID = "ext-res-1"
	const initialExtendCount = int64(0)

	fx := newExtensionReadFixture(t,
		http.StatusOK, canonicalReadBody("Ready", provisionUntilInsideWindow, initialExtendCount),
		http.StatusOK, extensionSuccessBodyWithExtraField,
	)
	srv := fx.Server()

	r := buildExtensionTestResource(t, srv.URL, fixedNowForExtensionTests)
	s := resourceSchemaForDelete(t)
	state := buildExtensionTestState(t, s, reservationID, extensionTestDurationDays, extensionTestWindowFraction, initialExtendCount, provisionUntilInsideWindow)

	req := resource.ReadRequest{State: state}
	resp := resource.ReadResponse{State: state}

	entries := captureAllTFLogEntries(t, func(ctx context.Context) {
		r.Read(ctx, req, &resp)
	})

	if resp.Diagnostics.HasError() {
		t.Fatalf("Read(): expected no error, got: %v", resp.Diagnostics)
	}

	// r2 finding 2: body_preview must be EXACTLY this pinned, struct-derived
	// format — never the raw body bytes (which would also contain the
	// unexpected_field sentinel below).
	const wantBodyPreview = `message="ok" status=200`

	found := false
	for _, e := range entries {
		if msg, _ := e["@message"].(string); msg != "Reservation extended" {
			continue
		}
		bp, ok := e["body_preview"]
		if !ok {
			t.Fatalf(`Read(): "Reservation extended" log entry is missing body_preview `+
				`(Hallazgo 5 — observability symmetry with the extensionAmbiguous branch): %v`, e)
		}
		bpStr, _ := bp.(string)
		if bpStr == "" {
			t.Error(`Read(): "Reservation extended" log entry's body_preview is present but empty`)
		}
		if bpStr != wantBodyPreview {
			t.Errorf(`Read(): "Reservation extended" log entry's body_preview = %q, want %q `+
				`(r2 finding 2: body_preview must be derived from the validated message/status `+
				`struct, not raw body bytes)`, bpStr, wantBodyPreview)
		}
		found = true
	}
	if !found {
		t.Fatal(`Read(): expected a "Reservation extended" log entry, found none`)
	}

	// r2 finding 2's core assertion: the raw body's unexpected extra field
	// must never leak into ANY log entry, at any level — mirrors this file's
	// existing sentinel-scan convention (runExtensionAuthOrRedirectBodyRedactedTest).
	for i, e := range entries {
		for k, v := range e {
			if strings.Contains(fmt.Sprintf("%v", v), extensionSuccessSentinelExtraField) {
				t.Errorf("r2 finding 2: log entry %d field %q leaked the 2xx body's unexpected "+
					"extra field content — body_preview must reflect only the validated "+
					"message/status struct fields, never raw body bytes: %v", i, k, v)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// r3 audit finding 3 (round 3, MEDIUM) — the r2 finding 2 fix above (deriving
// body_preview from the validated tzExtensionSuccessResponse struct instead
// of raw body bytes) dropped the implicit ~200-byte cap that
// truncate(extBody, 200) gave for free: fmt.Sprintf("message=%q status=%d",
// ok2xx.Message, ok2xx.Status) had no length limit on Message, so an
// unexpectedly large Message field produced an unbounded log line.
// ---------------------------------------------------------------------------

// TestExtensionSuccessBodyPreview_MessageIsTruncated: a huge (50,000-byte)
// Message field must still produce a bounded body_preview — re-applying
// truncate() (already used at 5 other call sites in this file) to
// ok2xx.Message specifically, before formatting.
//
// Exercises extensionSuccessBodyPreview directly (constructing the
// tzExtensionSuccessResponse value in-test) rather than driving it through
// the full Read() integration path: r3 audit finding 4 (in the same batch as
// this finding) hardened classifyExtensionResponse to require BOTH
// Status==200 AND Message=="ok" exactly, so a 50,000-byte, non-"ok" message
// can no longer reach this code path via classifyExtensionResponse at all —
// only "ok" (2 bytes) can ever classify as extensionSucceeded now. Testing
// the extraction directly keeps this regression test meaningful (confirming
// truncate() itself still bounds Message) independent of that now-stricter,
// separately-tested gate. See TestReadUnit_Extension_Succeeds_LogsBodyPreview_LargeMessageIsNowUnreachable
// below for the integration-level confirmation of that interaction.
func TestExtensionSuccessBodyPreview_MessageIsTruncated(t *testing.T) {
	t.Parallel()

	hugeMessage := strings.Repeat("A", 50000)
	got := extensionSuccessBodyPreview(tzExtensionSuccessResponse{Message: hugeMessage, Status: 200})

	// Generous margin over the ~200-byte truncate() cap plus the
	// `message=".." status=200` wrapper and any %q escaping overhead — the
	// point is proving bounded growth, not pinning an exact byte count.
	const maxAllowedLen = 250
	if len(got) > maxAllowedLen {
		t.Errorf("r3 audit finding 3: extensionSuccessBodyPreview output is %d bytes (input Message "+
			"was %d bytes) — want capped at roughly 200 bytes via truncate(), got unbounded growth: %.60s...",
			len(got), len(hugeMessage), got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("r3 audit finding 3: extensionSuccessBodyPreview output = %.60s... does not contain "+
			"the truncate() ellipsis marker, suggesting the 50,000-byte Message was not actually truncated", got)
	}
}

// TestExtensionSuccessBodyPreview_ShortMessagePassesThroughUnchanged: the
// confirmed-real success shape ("ok", 2 bytes — the only value that can
// reach this function via the legitimate Read() call path post-finding-4)
// must render byte-identical to before this fix, proving truncate() is a
// no-op for inputs already under the cap.
func TestExtensionSuccessBodyPreview_ShortMessagePassesThroughUnchanged(t *testing.T) {
	t.Parallel()
	const want = `message="ok" status=200`
	got := extensionSuccessBodyPreview(tzExtensionSuccessResponse{Message: "ok", Status: 200})
	if got != want {
		t.Errorf("extensionSuccessBodyPreview({Message:\"ok\",Status:200}) = %q, want %q", got, want)
	}
}

// TestReadUnit_Extension_Succeeds_LogsBodyPreview_LargeMessageIsNowUnreachable:
// documents, at the Read()-integration level, the interaction between r3
// audit findings 3 and 4 fixed in the same batch — a 2xx body with a huge,
// non-"ok" Message can no longer classify as extensionSucceeded at all
// (finding 4's hardening), so it now falls through to the existing
// no-blocking-error prune fallback instead of ever reaching the
// body_preview log line finding 3 protects. Confirms the two fixes compose
// correctly rather than silently reintroducing a bypass.
func TestReadUnit_Extension_Succeeds_LogsBodyPreview_LargeMessageIsNowUnreachable(t *testing.T) {
	t.Parallel()
	const reservationID = "ext-res-1"
	const initialExtendCount = int64(0)

	hugeMessage := strings.Repeat("A", 50000)
	hugeBody, err := json.Marshal(map[string]any{"message": hugeMessage, "status": 200})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	fx := newExtensionReadFixture(t,
		http.StatusOK, canonicalReadBody("Ready", provisionUntilInsideWindow, initialExtendCount),
		http.StatusOK, string(hugeBody),
	)
	srv := fx.Server()

	r := buildExtensionTestResource(t, srv.URL, fixedNowForExtensionTests)
	s := resourceSchemaForDelete(t)
	state := buildExtensionTestState(t, s, reservationID, extensionTestDurationDays, extensionTestWindowFraction, initialExtendCount, provisionUntilInsideWindow)

	req := resource.ReadRequest{State: state}
	resp := resource.ReadResponse{State: state}

	entries := captureAllTFLogEntries(t, func(ctx context.Context) {
		r.Read(ctx, req, &resp)
	})

	if resp.Diagnostics.HasError() {
		t.Fatalf("Read(): a non-classifiable extension response must never raise a blocking error, got: %v", resp.Diagnostics)
	}

	for _, e := range entries {
		if msg, _ := e["@message"].(string); msg == "Reservation extended" {
			t.Errorf(`Read(): got a "Reservation extended" log entry for a huge, non-"ok" Message — `+
				"post-finding-4, classifyExtensionResponse must reject this as extensionAmbiguous, "+
				"never extensionSucceeded: %v", e)
		}
	}
}

// ---------------------------------------------------------------------------
// Hallazgo 2 (audit round 1, HIGH) — distinguish "falls through to the
// independent PastExpiry re-evaluation" from "prunes unconditionally
// without re-evaluating."
//
// The 3 pre-existing *_FallsThroughToPrune tests above all use
// provisionUntilInsideWindow, which is ALSO past fixedNow — so a mutant that
// prunes unconditionally on any non-succeeded extension outcome (instead of
// correctly falling through to the existing, independent PastExpiry check)
// passes those 3 tests just the same. Shin empirically confirmed this gap by
// temporarily mutating the implementation to prune unconditionally: all 3
// tests still passed. This test uses a provisionUntil that is
// simultaneously extension-window-eligible (duration=4/fraction=0.5) AND
// strictly future relative to fixedNow (PastExpiry == false) — the two
// checks disagree, so only a correct fall-through-and-re-evaluate
// implementation keeps the reservation live.
// ---------------------------------------------------------------------------

// provisionUntilFutureInsideWindow: fixedNow + 24h. Independently verified
// this session (Hallazgo 2 remediation, never hand-calculated): windowSeconds
// = round(0.5*4*86400) = 172800s (2 days); windowStart = provisionUntilEpoch -
// 172800 = fixedNow - 24h; since fixedNow >= windowStart, InExtensionWindow
// returns eligible=true. Simultaneously, PastExpiry(provisionUntilFutureInsideWindow,
// fixedNow) == false because provisionUntil (fixedNow+24h) is strictly AFTER
// fixedNow (now < until, not now > until).
const provisionUntilFutureInsideWindow = "2026-07-21T12:00:00Z"

// TestReadUnit_Extension_RejectedButNotPastExpiry_RemainsLiveWithUnchangedEndDate:
// Hallazgo 2 remediation. A rejected extension attempt whose provisionUntil is
// still in the future (not past-expiry) must fall through to the existing,
// independent PastExpiry check and KEEP the reservation live — not be pruned
// unconditionally just because the extension attempt itself did not succeed.
func TestReadUnit_Extension_RejectedButNotPastExpiry_RemainsLiveWithUnchangedEndDate(t *testing.T) {
	t.Parallel()
	const reservationID = "ext-res-1"
	const initialExtendCount = int64(3)

	fx := newExtensionReadFixture(t,
		http.StatusOK, canonicalReadBody("Ready", provisionUntilFutureInsideWindow, initialExtendCount),
		http.StatusBadRequest, extensionRejectionLimitExhausted,
	)
	srv := fx.Server()

	r := buildExtensionTestResource(t, srv.URL, fixedNowForExtensionTests)
	s := resourceSchemaForDelete(t)
	state := buildExtensionTestState(t, s, reservationID, extensionTestDurationDays, extensionTestWindowFraction, initialExtendCount, provisionUntilFutureInsideWindow)

	req := resource.ReadRequest{State: state}
	resp := resource.ReadResponse{State: state}
	r.Read(context.Background(), req, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Read(): a rejected extension attempt must never raise a blocking error, got: %v", resp.Diagnostics)
	}
	if got := fx.PostCallCount(); got != 1 {
		t.Errorf("Read(): extension POST called %d times, want exactly 1", got)
	}

	// The critical assertion (Hallazgo 2): PastExpiry(provisionUntilFutureInsideWindow,
	// fixedNow) == false because provisionUntil is strictly in the future. A
	// mutant that prunes unconditionally on any non-succeeded extension
	// outcome (instead of falling through to this independent, unchanged
	// check) would incorrectly remove the resource here — which is exactly
	// what Shin's empirical mutation proved the 3 pre-existing tests could
	// not catch.
	if resp.State.Raw.IsNull() {
		t.Fatal("Read(): rejected extension with provisionUntil still in the future must fall through to the " +
			"existing PastExpiry check and KEEP the reservation — it was incorrectly removed from state " +
			"(this is the exact gap Hallazgo 2 catches: unconditional prune vs. correct re-evaluation)")
	}

	var got reservationModel
	if diags := resp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("resp.State.Get(): %v", diags)
	}
	if got.EndDate.ValueString() != provisionUntilFutureInsideWindow {
		t.Errorf("end_date after rejected extension = %q, want unchanged %q", got.EndDate.ValueString(), provisionUntilFutureInsideWindow)
	}
	if got.ExtendCount.ValueInt64() != initialExtendCount {
		t.Errorf("extend_count after rejected extension = %d, want unchanged %d (extension was rejected, not applied)",
			got.ExtendCount.ValueInt64(), initialExtendCount)
	}
}

// ---------------------------------------------------------------------------
// Hallazgo 3 (audit round 1, HIGH) — the extension POST's transport-error
// branch (extErr != nil — connectivity failure, not a completed HTTP
// response) had zero dedicated test coverage. Structurally identical code
// path to extensionAmbiguous (log + break/fall-through) but never exercised.
// ---------------------------------------------------------------------------

// TestReadUnit_Extension_POSTTransportError_FallsThroughToPrune: forces a
// genuine transport-layer failure specifically on the extension POST (the
// initial canonical GET succeeds normally), and confirms the same
// no-blocking-error fallback already proven for extensionAmbiguous /
// extensionNotPossible — AND that the production tflog.Warn on this exact
// path (resource_reservation.go:948-951) actually fires, matching its
// structurally-identical sibling
// TestReadUnit_Extension_AmbiguousResponse_FallsThroughToPrune_LogsWarn
// (r2 finding 4 — the plan's own must_have README D9/Q3 requires tflog.Warn
// on transport error, but this test previously verified only the
// no-blocking-error/exactly-1-POST/prune-fallthrough behavior, not the log
// emission itself).
func TestReadUnit_Extension_POSTTransportError_FallsThroughToPrune(t *testing.T) {
	t.Parallel()
	const reservationID = "ext-res-1"
	const initialExtendCount = int64(1)

	fx := newExtensionReadFixtureWithPostTransportError(t,
		http.StatusOK, canonicalReadBody("Ready", provisionUntilInsideWindow, initialExtendCount),
	)
	srv := fx.Server()

	r := buildExtensionTestResource(t, srv.URL, fixedNowForExtensionTests)
	s := resourceSchemaForDelete(t)
	state := buildExtensionTestState(t, s, reservationID, extensionTestDurationDays, extensionTestWindowFraction, initialExtendCount, provisionUntilInsideWindow)

	req := resource.ReadRequest{State: state}
	resp := resource.ReadResponse{State: state}

	warns := captureTFLogWarnings(t, func(ctx context.Context) {
		r.Read(ctx, req, &resp)
	})

	if resp.Diagnostics.HasError() {
		t.Fatalf("Read(): a POST transport error must never raise a blocking error, got: %v", resp.Diagnostics)
	}
	if got := fx.PostCallCount(); got != 1 {
		t.Errorf("Read(): extension POST attempted %d times, want exactly 1 (no in-process retry loop)", got)
	}
	if got := fx.GetCallCount(); got != 1 {
		t.Errorf("Read(): canonical GET called %d times, want exactly 1", got)
	}
	// provisionUntilInsideWindow is strictly before fixedNow (verified this
	// session) — the existing, unchanged PastExpiry check must prune it once
	// the failed extension attempt falls through.
	if !resp.State.Raw.IsNull() {
		t.Error("Read(): a POST transport error must fall through to the existing PastExpiry prune — " +
			"resource should have been removed from state, but it was not")
	}
	if len(warns) == 0 {
		t.Error("Read(): a POST transport error MUST be logged at tflog.Warn — " +
			"must_have README D9/Q3 requires this, and its structurally-identical sibling " +
			"(AmbiguousResponse_FallsThroughToPrune_LogsWarn) already asserts the same for " +
			"the adjacent extensionAmbiguous path; silence here would be a sibling-inconsistent " +
			"coverage gap (r2 finding 4)")
	}
}

// TestReadUnit_Extension_NotYetEligible_NoAttemptMade: the routine, most
// common outcome — provisionUntil far in the future, well outside the
// window. No extension attempt (no POST at all); normal Read() proceeds
// unchanged.
func TestReadUnit_Extension_NotYetEligible_NoAttemptMade(t *testing.T) {
	t.Parallel()
	const reservationID = "ext-res-1"
	const initialExtendCount = int64(0)

	fx := newExtensionReadFixture(t,
		http.StatusOK, canonicalReadBody("Ready", provisionUntilFarFuture, initialExtendCount),
		http.StatusOK, extensionSuccessBody, // must never be hit
	)
	srv := fx.Server()

	r := buildExtensionTestResource(t, srv.URL, fixedNowForExtensionTests)
	s := resourceSchemaForDelete(t)
	state := buildExtensionTestState(t, s, reservationID, extensionTestDurationDays, extensionTestWindowFraction, initialExtendCount, provisionUntilFarFuture)

	req := resource.ReadRequest{State: state}
	resp := resource.ReadResponse{State: state}
	r.Read(context.Background(), req, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Read(): expected no error, got: %v", resp.Diagnostics)
	}
	if got := fx.PostCallCount(); got != 0 {
		t.Errorf("Read(): extension POST called %d times, want exactly 0 (not yet eligible — the routine majority case must never attempt extension)", got)
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("Read(): resource must not be removed — provisionUntil is far in the future")
	}

	var got reservationModel
	if diags := resp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("resp.State.Get(): %v", diags)
	}
	if got.EndDate.ValueString() != provisionUntilFarFuture {
		t.Errorf("end_date = %q, want unchanged %q (no extension attempted)", got.EndDate.ValueString(), provisionUntilFarFuture)
	}
	if got.ExtendCount.ValueInt64() != initialExtendCount {
		t.Errorf("extend_count = %d, want unchanged %d", got.ExtendCount.ValueInt64(), initialExtendCount)
	}
}

// TestReadUnit_Extension_CannotDetermineEligibility_SkipsAttempt_LogsWarn:
// an unparseable provisionUntil in the API response (e.g. JSON null) means
// InExtensionWindow returns ok=false ("cannot determine") — extension is
// skipped (no POST), and this MUST be observable via tflog.Warn, never
// silent. Falls through to the existing (unchanged) PastExpiry, which ALSO
// cannot determine and therefore KEEPs (matches the pre-existing,
// independently-fixed fail-closed contract from Phase A).
func TestReadUnit_Extension_CannotDetermineEligibility_SkipsAttempt_LogsWarn(t *testing.T) {
	t.Parallel()
	const reservationID = "ext-res-1"
	const initialExtendCount = int64(0)
	const unparseableProvisionUntil = "" // decoded from JSON null via derefString

	fx := newExtensionReadFixture(t,
		http.StatusOK, canonicalReadBody("Ready", unparseableProvisionUntil, initialExtendCount),
		http.StatusOK, extensionSuccessBody, // must never be hit
	)
	srv := fx.Server()

	r := buildExtensionTestResource(t, srv.URL, fixedNowForExtensionTests)
	s := resourceSchemaForDelete(t)
	state := buildExtensionTestState(t, s, reservationID, extensionTestDurationDays, extensionTestWindowFraction, initialExtendCount, unparseableProvisionUntil)

	req := resource.ReadRequest{State: state}
	resp := resource.ReadResponse{State: state}

	warns := captureTFLogWarnings(t, func(ctx context.Context) {
		r.Read(ctx, req, &resp)
	})

	if resp.Diagnostics.HasError() {
		t.Fatalf("Read(): expected no error, got: %v", resp.Diagnostics)
	}
	if got := fx.PostCallCount(); got != 0 {
		t.Errorf("Read(): extension POST called %d times, want exactly 0 (eligibility could not be determined)", got)
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("Read(): resource must not be removed — PastExpiry also cannot determine (unparseable provisionUntil) and therefore KEEPs, per the existing fail-closed contract")
	}
	if len(warns) == 0 {
		t.Error("Read(): 'cannot determine extension eligibility' MUST be logged at tflog.Warn — " +
			"this is the deliberate mitigation for the fail-silent-to-KEEP pattern " +
			"(gotchas/ibm-techzone.md); silently skipping here with no log reproduces exactly " +
			"the class of bug this plan exists to close")
	}
}

// ---------------------------------------------------------------------------
// r3 audit finding 6 (round 3, MEDIUM) — every Read()-integration test above
// uses the identical fixed pair (extensionTestDurationDays=4,
// extensionTestWindowFraction=0.5 — 0.5 is literally
// techzone.DefaultExtensionWindowFraction), never varying either value. This
// cannot distinguish "Read() genuinely reads durationDays/windowFraction
// from state" from "these two parameters are coincidentally hardcoded
// somewhere and happen to match the constants used everywhere else" — the
// same bug class r1's HIGH #2 already fixed elsewhere, never applied to
// these two parameters at the Read()-integration level.
// ---------------------------------------------------------------------------

// nonDefaultDurationDays / nonDefaultWindowFraction: deliberately BOTH
// different from extensionTestDurationDays(4)/extensionTestWindowFraction(0.5)
// used everywhere else in this file. windowFraction=1.0 is also the
// validator's own inclusive upper boundary (a case separately flagged as a
// dedicated-test-coverage gap for InExtensionWindow itself in
// extension_window_test.go — this Read()-integration test is a distinct,
// additional angle: proving Read() passes THIS value through, not that the
// boundary itself is correct).
const nonDefaultDurationDays = int64(7)
const nonDefaultWindowFraction = 1.0

// provisionUntilNonDefaultInsideWindow: fixedNow + 3 days. Independently
// verified via Go's own math/time packages in this environment (never
// hand-calculated, per plan README Decision D5's discipline): windowSeconds
// = round(1.0*7*86400) = 604800s (7 days); windowStart = provisionUntilEpoch
// - 604800 = fixedNow - 4 days ("2026-07-16T12:00:00Z"); since fixedNow
// (2026-07-20T12:00:00Z) >= windowStart, InExtensionWindow returns
// eligible=true for the CORRECT (durationDays=7, fraction=1.0) pair.
//
// Discriminator check (the actual point of this test): with the WRONG
// values instead — the extensionTestDurationDays(4)/extensionTestWindowFraction(0.5)
// pair used everywhere else in this file — windowSeconds would instead be
// round(0.5*4*86400) = 172800s (2 days), giving windowStart =
// provisionUntilEpoch - 172800 = fixedNow + 1 day, which is AFTER fixedNow —
// eligible would incorrectly be FALSE. A Read() that silently hardcoded or
// mis-wired durationDays/windowFraction to the default pair instead of
// reading them from state would therefore attempt ZERO extension POSTs here
// instead of the expected one — precisely the wiring bug this finding
// targets, made observable via PostCallCount().
const provisionUntilNonDefaultInsideWindow = "2026-07-23T12:00:00Z"

// wantNextExtensionDateNonDefault: NextExtensionDate(provisionUntilNonDefaultInsideWindow, 7).
// Independently verified via Go's time package (this session):
// time.Date(2026,7,23,12,0,0,0,UTC).Add(7*24h) == 2026-07-30T12:00:00.000Z.
const wantNextExtensionDateNonDefault = "2026-07-30T12:00:00.000Z"

// TestReadUnit_Extension_Succeeds_NonDefaultDurationAndWindowFraction: same
// shape as TestReadUnit_Extension_Succeeds_UpdatesState, but with BOTH
// durationDays and windowFraction set to values that differ from the
// default/fixed pair used by every other Read()-integration test in this
// file — proving Read() genuinely reads both from state rather than from a
// hardcoded stand-in (see the discriminator arithmetic in the constants'
// doc comments above).
func TestReadUnit_Extension_Succeeds_NonDefaultDurationAndWindowFraction(t *testing.T) {
	t.Parallel()
	const reservationID = "ext-res-1"
	const initialExtendCount = int64(5)

	fx := newExtensionReadFixture(t,
		http.StatusOK, canonicalReadBody("Ready", provisionUntilNonDefaultInsideWindow, initialExtendCount),
		http.StatusOK, extensionSuccessBody,
	)
	srv := fx.Server()

	r := buildExtensionTestResource(t, srv.URL, fixedNowForExtensionTests)
	s := resourceSchemaForDelete(t)
	state := buildExtensionTestState(t, s, reservationID, nonDefaultDurationDays, nonDefaultWindowFraction, initialExtendCount, provisionUntilNonDefaultInsideWindow)

	req := resource.ReadRequest{State: state}
	resp := resource.ReadResponse{State: state}
	r.Read(context.Background(), req, &resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Read(): expected no error, got: %v", resp.Diagnostics)
	}

	// The core discriminator assertion: exactly 1 POST. If Read() silently
	// used the default (4, 0.5) pair instead of state's real (7, 1.0)
	// values, InExtensionWindow would return eligible=false for this exact
	// provisionUntil (see the constant's doc comment above) and NO POST
	// would ever be sent.
	if got := fx.PostCallCount(); got != 1 {
		t.Fatalf("Read(): extension POST called %d times, want exactly 1 — a count of 0 here would mean "+
			"Read() is not genuinely reading durationDays=%d/windowFraction=%v from state (r3 audit "+
			"finding 6): those exact non-default values are REQUIRED to make this reservation eligible; "+
			"the default pair (durationDays=4/windowFraction=0.5) would NOT be eligible for the same "+
			"provisionUntil", got, nonDefaultDurationDays, nonDefaultWindowFraction)
	}

	if resp.State.Raw.IsNull() {
		t.Fatal("Read(): resource must not be removed — extension succeeded, so PastExpiry is skipped entirely")
	}

	var got reservationModel
	if diags := resp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("resp.State.Get(): %v", diags)
	}
	if got.EndDate.ValueString() != wantNextExtensionDateNonDefault {
		t.Errorf("end_date after successful non-default extension = %q, want %q (NextExtensionDate("+
			"%q, %d) — proves the SAME non-default durationDays also reaches NextExtensionDate, not "+
			"just InExtensionWindow)",
			got.EndDate.ValueString(), wantNextExtensionDateNonDefault, provisionUntilNonDefaultInsideWindow, nonDefaultDurationDays)
	}
	if got.ExtendCount.ValueInt64() != initialExtendCount+1 {
		t.Errorf("extend_count after successful extension = %d, want %d", got.ExtendCount.ValueInt64(), initialExtendCount+1)
	}
}
