/**
 * @spec-handoff — Audit Remediation (Round 1) — payload.go contracts
 *
 * @interface BuildCreatePayload(platformRaw json.RawMessage, dynamicOutputs map[string]string, in CreateInput) ([]byte, error)
 *
 * @behavior
 *   - MUST return a non-nil error if any key in dynamicOutputs collides with a
 *     reserved structural payload field. Reserved set (minimum, from Ei F-02):
 *       "platform", "user", "type", "terms", "collectionId", "name", "purpose",
 *       "region", "datacenter", "cloudAccount", "dynamicOutputs", "start", "end",
 *       "template", "requestMethod", "infrastructure", "iui", "opportunity".
 *     Non-colliding keys (e.g. "_03_key", "_04_value") MUST succeed unchanged.
 *   - MUST return a non-nil error if platformRaw is not valid JSON (json.Valid check
 *     at entry point). This makes the contract explicit and prevents silent pass-through
 *     if the two-pass collection decode is ever bypassed.
 *
 * @edge-cases
 *   - Key colliding with "platform"     → error (most dangerous: overwrites json.RawMessage)
 *   - Key colliding with "dynamicOutputs" → error (structural breakage)
 *   - Key colliding with "name"          → error
 *   - Key colliding with "type"          → error
 *   - Key colliding with "terms"         → error
 *   - Non-colliding _NN_ keys only       → success, payload unchanged
 *   - platformRaw = []byte("not json")   → error
 *   - platformRaw = []byte(`{}`)         → success (valid JSON)
 *
 * @contracts-for-kou
 *   1. Define a reservedPayloadKeys set (map[string]struct{}) containing at minimum
 *      the names listed above.
 *   2. Before the flat-key emission loop, iterate sortedKeys and return
 *      fmt.Errorf("dynamic_outputs key %q collides with reserved payload field", k)
 *      for any key in the reserved set.
 *   3. At the top of BuildCreatePayload, add:
 *        if !json.Valid(platformRaw) {
 *            return nil, fmt.Errorf("platformRaw is not valid JSON")
 *        }
 *
 * @see ./payload.go
 * @see ../../.yui-soul/reviews/114-techzone-template-agnostic/r1-ei-injection.md (F-02)
 */

// Package techzone_test — audit remediation RED tests for payload.go.
//
// RED GATE: These tests FAIL against the current payload.go because:
//   (a) BuildCreatePayload has no reserved-key check — colliding keys silently
//       overwrite structural fields; the test expects an error but gets nil.
//   (b) BuildCreatePayload has no json.Valid guard on platformRaw — invalid bytes
//       are passed through; the test expects an error but gets nil (or a json.Marshal
//       error on the outer payload, which is the wrong place and wrong error).
//
// Goes GREEN when Kou adds the collision guard and json.Valid guard to payload.go.
package techzone_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shoootyou-ext/terraform-provider-ibmtechzone/internal/techzone"
)

// minimalInput is a valid CreateInput fixture for collision/validation tests.
// Uses frozen timestamps — never time.Now().
var minimalInput = techzone.CreateInput{
	Name:          "remediation-test",
	Purpose:       "Demo",
	Region:        "us-east-2",
	Datacenter:    "",
	CollectionID:  "69650af0758b9e41de66b6ae",
	Template:      "aws-account-hashicorp-ddr",
	RequestMethod: "aws-account-hashicorp-ddr",
	CloudAccount:  "ITZ",
	Start:         "2026-01-01T00:00:01.000Z",
	End:           "2026-01-02T00:01:01.000Z",
}

// validPlatformRaw is a minimal valid JSON object for use as the platformRaw fixture.
var validPlatformRaw = json.RawMessage(`{"infrastructure":"aws","regions":[]}`)

// ---------------------------------------------------------------------------
// Contract 1 — dynamic_outputs key-clobbering guard
// ---------------------------------------------------------------------------

// TestBuildCreatePayload_CollidingKey_ReturnsError verifies that BuildCreatePayload
// returns a non-nil error when a dynamic_outputs key collides with any reserved
// structural payload field.
//
// RED: current BuildCreatePayload has no reserved-key check. The colliding key
// silently overwrites the structural field. No error is returned.
// This test will FAIL with "expected non-nil error for colliding key..., got nil".
func TestBuildCreatePayload_CollidingKey_ReturnsError(t *testing.T) {
	t.Parallel()

	// Reserved field names that dynamic_outputs keys must not collide with.
	// Sourced from Ei audit finding F-02 and the actual payload map in payload.go.
	collidingKeys := []string{
		"platform",        // overwrites json.RawMessage → structural breakage
		"user",            // absent by contract (E8), but must be blocked
		"type",            // fixed constant "reservation"
		"terms",           // fixed bool true — overwrite with string causes breakage
		"collectionId",    // identity field
		"name",            // reservation name
		"purpose",         // purpose field
		"region",          // region field
		"datacenter",      // datacenter field
		"cloudAccount",    // cloud account field
		"dynamicOutputs",  // the array itself — catastrophic structural clobber
		"start",           // ISO-8601 timestamp
		"end",             // ISO-8601 timestamp
		"template",        // template derived from collection
		"requestMethod",   // requestMethod from collection
		"infrastructure",  // fixed constant "aws"
		"iui",             // optional field
		"opportunity",     // optional field
	}

	for _, collidingKey := range collidingKeys {
		collidingKey := collidingKey // capture for parallel sub-test
		t.Run("collides_with_"+collidingKey, func(t *testing.T) {
			t.Parallel()

			dynOutputs := map[string]string{
				collidingKey: "injected-value",
				"_03_safe":   "safe-value", // a non-colliding key in the same map
			}

			_, err := techzone.BuildCreatePayload(validPlatformRaw, dynOutputs, minimalInput)
			if err == nil {
				t.Errorf(
					"FAIL: expected non-nil error for colliding key %q, got nil\n"+
						"  BuildCreatePayload must return an error when a dynamic_outputs key\n"+
						"  collides with a reserved structural payload field.",
					collidingKey,
				)
			}
		})
	}
}

// TestBuildCreatePayload_NonCollidingKeys_Succeeds verifies that BuildCreatePayload
// does NOT error when all dynamic_outputs keys are non-colliding _NN_ keys.
//
// This is the counter-case: the guard must only block colliding keys, not all keys.
// GREEN against current code (no guard → always succeeds), but remains GREEN after
// the guard is added because _NN_ keys don't match reserved names.
func TestBuildCreatePayload_NonCollidingKeys_Succeeds(t *testing.T) {
	t.Parallel()

	dynOutputs := map[string]string{
		"_03_account_cleanup": "true",
		"_04_hcp_org":         "test-org",
		"_05_hcp_project":     "test-project",
	}

	gotBytes, err := techzone.BuildCreatePayload(validPlatformRaw, dynOutputs, minimalInput)
	if err != nil {
		t.Fatalf(
			"FAIL: BuildCreatePayload returned unexpected error for non-colliding _NN_ keys: %v",
			err,
		)
	}
	if len(gotBytes) == 0 {
		t.Fatal("FAIL: BuildCreatePayload returned empty bytes on success")
	}

	// Verify the _NN_ keys are present in the output (dual-emit contract).
	var got map[string]any
	if err := json.Unmarshal(gotBytes, &got); err != nil {
		t.Fatalf("FAIL: output is not valid JSON: %v", err)
	}
	for k, wantV := range dynOutputs {
		gotV, ok := got[k].(string)
		if !ok {
			t.Errorf("FAIL: flat key %q absent or not a string in output", k)
			continue
		}
		if gotV != wantV {
			t.Errorf("FAIL: flat key %q = %q, want %q", k, gotV, wantV)
		}
	}
}

// TestBuildCreatePayload_SingleCollidingKey_MapWithOnlyCollidingKey verifies
// that even a single-entry map with a colliding key is rejected.
//
// RED: same as above — no guard exists yet.
func TestBuildCreatePayload_SingleCollidingKey_MapWithOnlyCollidingKey(t *testing.T) {
	t.Parallel()

	// "platform" is the most dangerous collision: replaces json.RawMessage with a string.
	dynOutputs := map[string]string{
		"platform": "malicious-override",
	}

	_, err := techzone.BuildCreatePayload(validPlatformRaw, dynOutputs, minimalInput)
	if err == nil {
		t.Errorf(
			"FAIL: expected error for dynamic_outputs key \"platform\" (most dangerous collision — " +
				"replaces json.RawMessage with a string, causing TechZone 500 per gotchas/techzone.md), " +
				"got nil",
		)
	}
}

// ---------------------------------------------------------------------------
// Contract 2 — platform json.Valid guard
// ---------------------------------------------------------------------------

// TestBuildCreatePayload_InvalidPlatformRaw_ReturnsExplicitError verifies that
// BuildCreatePayload returns an explicit, early error (not a json.Marshal error)
// when platformRaw is not valid JSON.
//
// Context: json.Marshal of a json.RawMessage already errors when the bytes are
// invalid JSON — so BuildCreatePayload currently DOES return an error for invalid
// platformRaw (via json.Marshal). However, the contract requires an EXPLICIT
// json.Valid guard at the entry point of BuildCreatePayload, so that:
//   (a) The error fires BEFORE any other processing (before dynamic-outputs collision
//       checks, before payload map construction, before marshal).
//   (b) The error message clearly identifies the problem as "platformRaw is not valid JSON"
//       rather than an opaque "error calling MarshalJSON for type json.RawMessage: ...".
//
// RED: the current implementation returns an error but it is NOT the explicit entry-point
// error. The error message contains "MarshalJSON" or "unexpected end of JSON input" rather
// than a clear "platformRaw is not valid JSON" message.
//
// Kou must add at the TOP of BuildCreatePayload, before any other logic:
//   if !json.Valid(platformRaw) {
//       return nil, fmt.Errorf("platformRaw is not valid JSON")
//   }
//
// The test validates that the returned error's message matches the explicit contract
// string (contains "platformRaw" or "not valid JSON"), NOT the generic marshal message.
func TestBuildCreatePayload_InvalidPlatformRaw_ReturnsExplicitError(t *testing.T) {
	t.Parallel()

	invalidPayloads := []struct {
		name  string
		bytes json.RawMessage
	}{
		{"plain_text", json.RawMessage(`not json at all`)},
		{"truncated_object", json.RawMessage(`{"key": "val`)},
		{"bare_string_no_quotes", json.RawMessage(`hello`)},
		{"empty_bytes", json.RawMessage(``)},
	}

	for _, tc := range invalidPayloads {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := techzone.BuildCreatePayload(tc.bytes, nil, minimalInput)
			if err == nil {
				t.Errorf(
					"FAIL: expected non-nil error for invalid platformRaw %q, got nil",
					string(tc.bytes),
				)
				return
			}

			// The error MUST be an explicit entry-point validation error, not a
			// json.Marshal error. The message must mention "platformRaw" or "not valid JSON"
			// to be considered an explicit guard (not a marshal-time accident).
			//
			// RED: current error is from json.Marshal internals, e.g.:
			//   "json: error calling MarshalJSON for type json.RawMessage: ..."
			// or:
			//   "json: error calling MarshalJSON for type json.RawMessage: unexpected end..."
			// The test asserts the message identifies the contract explicitly.
			errMsg := err.Error()
			hasExplicitMsg := strings.Contains(errMsg, "platformRaw") ||
				strings.Contains(errMsg, "not valid JSON") ||
				strings.Contains(errMsg, "invalid platform")
			if !hasExplicitMsg {
				t.Errorf(
					"FAIL: error for invalid platformRaw %q is not an explicit entry-point guard\n"+
						"  got error: %q\n"+
						"  want: error message containing 'platformRaw', 'not valid JSON', or 'invalid platform'\n"+
						"  BuildCreatePayload must call json.Valid(platformRaw) at entry and return\n"+
						"  fmt.Errorf(\"platformRaw is not valid JSON\") — NOT rely on json.Marshal to\n"+
						"  detect the problem implicitly.",
					string(tc.bytes), errMsg,
				)
			}
		})
	}
}

// TestBuildCreatePayload_ValidPlatformRaw_Succeeds confirms that a valid JSON
// platformRaw does NOT trigger the guard.
//
// This is the counter-case. Remains GREEN before and after the guard is added.
func TestBuildCreatePayload_ValidPlatformRaw_Succeeds(t *testing.T) {
	t.Parallel()

	validCases := []struct {
		name  string
		bytes json.RawMessage
	}{
		{"empty_object", json.RawMessage(`{}`)},
		{"minimal_object", json.RawMessage(`{"infrastructure":"aws"}`)},
		{"full_fixture", canonicalPlatformRaw},
	}

	for _, tc := range validCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := techzone.BuildCreatePayload(tc.bytes, nil, minimalInput)
			if err != nil {
				t.Errorf(
					"FAIL: BuildCreatePayload returned unexpected error for valid platformRaw %q: %v",
					string(tc.bytes), err,
				)
			}
		})
	}
}
