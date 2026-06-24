/**
 * @spec-handoff
 *
 * @interface BuildCreatePayload(platformRaw json.RawMessage, dynamicOutputs map[string]string, in CreateInput) ([]byte, error)
 *
 * @interface CreateInput struct {
 *   Name, Purpose, Region, Datacenter, CollectionID string
 *   Template, RequestMethod, CloudAccount           string
 *   Start, End                                      string  // ISO-8601
 *   User        string   // REQUIRED — reservation owner email (sent as "user" in payload)
 *   Opportunity []string // optional — emitted as JSON array when len>0; omitted when empty
 *   IUI         string   // optional — emitted as string when non-empty; omitted when ""
 * }
 *
 * @behavior
 *   - Assembles the full JSON body for POST /api/reservation/aws.
 *   - `platform` key in the output is set to platformRaw VERBATIM — bytes are
 *     embedded byte-for-byte via json.RawMessage's json.Marshaler, with NO
 *     re-encoding or field reordering.
 *   - `dynamicOutputs` array is emitted in LEXICOGRAPHIC order by the `name`
 *     field using an explicit sort.Strings (NEVER relying on map-iteration order).
 *   - For every entry in dynamicOutputs, a flat top-level key `name: value` is
 *     ALSO emitted (dual-emit: both array and flat keys, same lexicographic order).
 *   - `user` key MUST be present in the output payload and equal to in.User.
 *     Live API rejects the request (HTTP 500 "Invalid user assignment") when `user`
 *     is absent. The server stores `user:""` but uses the submitted value for
 *     assignment (stored as `myId`). `user` is always emitted — it is never omitted.
 *   - `opportunity` key is emitted as a JSON ARRAY ([]string) when
 *     len(CreateInput.Opportunity) > 0; OMITTED entirely when the slice is empty.
 *     Sending a string instead of an array causes the API to return HTTP 400.
 *   - `iui` key is ABSENT when CreateInput.IUI is "" (omitted); emitted as a
 *     JSON string when non-empty.
 *   - `description` is ALWAYS emitted as the constant string
 *     "Terraform-managed reservation".
 *   - Fixed constants kept from current implementation:
 *       reservationpurpose-0 = "Demo"
 *       accountPool          = "any"
 *       geo                  = "any"
 *       customer             = ""
 *       infrastructure       = "aws"
 *       type                 = "reservation"
 *       reservationtype-0    = "reservation"
 *       terms                = true
 *       customerData         = "false"
 *       customerDataTypes    = []
 *       opportunityProduct   = []
 *       notes                = ""
 *   - `template` and `requestMethod` are taken from CreateInput.Template and
 *     CreateInput.RequestMethod (derived from collection by caller, not hardcoded).
 *   - `region` is taken from CreateInput.Region; `datacenter` from CreateInput.Datacenter.
 *   - `cloudAccount` is taken from CreateInput.CloudAccount.
 *   - `collectionId` is taken from CreateInput.CollectionID.
 *   - `start` and `end` are taken from CreateInput.Start / CreateInput.End (must be
 *     injected frozen values — NEVER time.Now()).
 *
 * @edge-cases
 *   - Empty dynamicOutputs map (nil or zero-length) → "dynamicOutputs": [] emitted,
 *     NO flat _NN_ keys present in output.
 *   - Opportunity = nil / [] → `opportunity` key absent from payload.
 *   - Opportunity = ["006Ka..."] → `opportunity` key emitted as JSON array.
 *   - IUI = "" → `iui` key absent from payload.
 *   - platformRaw with specific key ordering → output preserves that ordering verbatim
 *     (no round-trip through map[string]any; json.RawMessage guarantees byte identity).
 *
 * @see ./payload.go       (implementation — must be updated by Kou to match this spec)
 * @see ../plans/wip/114-techzone-template-agnostic/e2-spec-platform-raw.md  (locked spec)
 * @see ../rfcs/approved/022-techzone-template-agnostic/README.md             (golden test spec)
 */

// Package techzone_test — golden/characterization test for BuildCreatePayload.
//
// RED GATE (Plan 114 live-validation fix): these tests MUST fail against the current
// payload.go because:
//   (a) CreateInput.User does not exist yet → compile error on the struct literal.
//   (b) CreateInput.Opportunity is `string`, not `[]string` → type mismatch compile error.
//   (c) Even if it compiled, the `user` key is absent from the payload map (E8 decision
//       was wrong) → assertion TestBuildCreatePayload_Golden/user_present_equals_User fails.
//   (d) `description` constant is absent from the payload → assertion fails.
//   (e) `opportunity` array emission logic is absent / wrong type → assertion fails.
//
// A test that was never RED proved nothing. Commit first; Kou's fix follows.
package techzone_test

import (
	"bytes"
	"encoding/json"
	"sort"
	"testing"

	"github.com/shoootyou-ext/terraform-provider-ibmtechzone/internal/techzone"
)

// ---------------------------------------------------------------------------
// Frozen fixtures — NEVER time.Now()
// ---------------------------------------------------------------------------

// frozenStart and frozenEnd are the canonical frozen timestamps injected into
// all golden test cases (RFC golden-test spec, E10 evidence — determinism requirement).
const (
	frozenStart = "2026-01-01T00:00:01.000Z"
	frozenEnd   = "2026-01-02T00:01:01.000Z"
)

// canonicalPlatformRaw is the fixture representing the verbatim bytes returned
// by GET /api/collection/69650af0758b9e41de66b6ae for platforms[0].
//
// Key ordering is intentional and non-alphabetical ("oid" before "id") to prove
// that BuildCreatePayload embeds the bytes verbatim — if it round-tripped through
// map[string]any, the key order would be sorted alphabetically by encoding/json
// and the byte-identity assertion would fail (exactly what we want to catch).
var canonicalPlatformRaw = json.RawMessage(`{"oid":"69650af0758b9e41de66b6ae","id":"69651c138d6e497dc77a8dbe","name":"Reservation Name","description":"Reservation Name","automationBucket":"public-solutions","infrastructure":"aws","regions":[{"name":"US East 2","template":"aws-account-hashicorp-ddr","requestMethod":"aws-account-hashicorp-ddr","cloudAccount":"ITZ","geo":"","region":"us-east-2","datacenter":"","status":"Enabled","pattern":{"id":"ccp-gitops/aws-account-hashicorp-ddr/itz","name":"aws-account-hashicorp-ddr","profile":"default"},"variables":[],"infrastructure":"aws","profile":"default"}],"status":"Enabled","createdAt":1768234003783,"updatedAt":1768235306788}`)

// ---------------------------------------------------------------------------
// TestBuildCreatePayload_Golden
// ---------------------------------------------------------------------------

// TestBuildCreatePayload_Golden is the characterization/golden test for the
// REFACTORED BuildCreatePayload signature (Phase 1, RFC 022 / Plan 114 E3).
//
// RED requirement: this test MUST fail against the OLD payload.go because the
// new signature is:
//
//	BuildCreatePayload(platformRaw json.RawMessage, dynamicOutputs map[string]string, in CreateInput) ([]byte, error)
//
// The old signature is:
//
//	BuildCreatePayload(in CreateInput) ([]byte, error)
//
// The extra leading arguments cause an immediate compile error. That is the
// correct red signal — it pins the target contract for Kou.
func TestBuildCreatePayload_Golden(t *testing.T) {
	t.Parallel()

	// canonicalDDRInputs is the locked DDR input vector from the RFC golden-test spec
	// (E1 live capture id 6a3c170cdbec3f43994128f6, sanitized).
	// All three _NN_ dynamic outputs are USER-SUPPLIED (E2 decision B1: no provider
	// defaults, not even _03_account_cleanup).
	canonicalDDRDynamicOutputs := map[string]string{
		"_03_account_cleanup": "true",
		"_04_hcp_org":         "golden-org",
		"_05_hcp_project":     "golden-project",
	}

	canonicalDDRInput := techzone.CreateInput{
		Name:          "golden-ddr",
		Purpose:       "Demo",
		Region:        "us-east-2",
		Datacenter:    "",
		CollectionID:  "69650af0758b9e41de66b6ae",
		Template:      "aws-account-hashicorp-ddr",
		RequestMethod: "aws-account-hashicorp-ddr",
		CloudAccount:  "ITZ",
		Start:         frozenStart,
		End:           frozenEnd,
		// User MUST be set — live API returns HTTP 500 when absent (Plan 114 live fix).
		// E8 decision was a misread: server uses the submitted value for myId assignment.
		User: "tester@example.com",
		// Opportunity and IUI are intentionally zero-value (→ absent from payload).
		// Opportunity: nil / [] → omitted (tested separately below).
	}

	t.Run("canonical_DDR_full_dynamic_outputs", func(t *testing.T) {
		t.Parallel()

		gotBytes, err := techzone.BuildCreatePayload(canonicalPlatformRaw, canonicalDDRDynamicOutputs, canonicalDDRInput)
		if err != nil {
			t.Fatalf("BuildCreatePayload returned unexpected error: %v", err)
		}

		// Unmarshal for structural assertions.
		var got map[string]any
		if err := json.Unmarshal(gotBytes, &got); err != nil {
			t.Fatalf("output is not valid JSON: %v\nbytes: %s", err, gotBytes)
		}

		// --- A: user key MUST be present and equal to CreateInput.User ---
		//
		// Live-validation finding (Plan 114): the API returns HTTP 500 "Invalid user
		// assignment" when `user` is absent. E8 decision was a misread — the server
		// stores `user:""` but uses the submitted value for assignment (stored as myId).
		// `user` must always be emitted; it equals in.User = "tester@example.com".
		//
		// RED: current payload.go does not include `user` in the payload map.
		assertStringField(t, got, "user", "tester@example.com")

		// --- B: start / end frozen timestamps ---
		assertStringField(t, got, "start", frozenStart)
		assertStringField(t, got, "end", frozenEnd)

		// --- C: name and purpose ---
		assertStringField(t, got, "name", "golden-ddr")
		assertStringField(t, got, "purpose", "Demo")

		// --- D: platform — RAW BYTE-IDENTITY (audit remediation item 7) ---
		//
		// TIGHTENED ASSERTION: We compare the raw JSON bytes of the "platform" value
		// directly against canonicalPlatformRaw, WITHOUT any unmarshal-remarshal cycle.
		//
		// Previous implementation (WRONG): both sides went through json.Unmarshal →
		// map[string]any → json.Marshal, which normalized key order alphabetically on
		// BOTH sides — masking the reorder regression this test is supposed to catch.
		// (Sho-core finding #1, Shin audit F-04)
		//
		// Correct implementation: extract the raw "platform" bytes from the outer JSON
		// payload bytes using bytes.Index, then compare directly against canonicalPlatformRaw.
		//
		// KEY ORDER CONTRACT: canonicalPlatformRaw has "oid" before "id" (non-alphabetical).
		// If BuildCreatePayload decoded platformRaw into map[string]any and re-encoded,
		// encoding/json would sort keys to "id" before "oid". The raw-byte comparison
		// detects this regression immediately — the bytes would NOT match because the
		// re-encoded form has alphabetical order.
		//
		// The assertion uses bytes.Contains(gotBytes, canonicalPlatformRaw) rather than
		// extracting and comparing the full sub-JSON, which is simpler and equally precise:
		// the canonical raw bytes must appear verbatim as a subsequence in the output.
		if !bytes.Contains(gotBytes, []byte(canonicalPlatformRaw)) {
			t.Errorf(
				"FAIL: platform bytes are NOT verbatim in the output payload\n"+
					"  canonicalPlatformRaw (non-alphabetical key order, 'oid' before 'id') "+
					"must appear byte-for-byte in the marshalled payload.\n"+
					"  If BuildCreatePayload round-tripped platformRaw through map[string]any,\n"+
					"  encoding/json would have alphabetized the keys, making 'id' appear before 'oid'.\n"+
					"  This assertion catches that regression.\n"+
					"  want (canonical raw): %s\n"+
					"  got (full output):    %s",
				canonicalPlatformRaw, gotBytes,
			)
		}

		// --- E: dynamicOutputs array — lexicographically sorted (E10) ---
		dynRaw, ok := got["dynamicOutputs"]
		if !ok {
			t.Fatalf("FAIL: `dynamicOutputs` key is absent from payload")
		}
		dynSlice, ok := dynRaw.([]any)
		if !ok {
			t.Fatalf("FAIL: `dynamicOutputs` is not a JSON array, got %T", dynRaw)
		}

		// Build the expected sorted array from the canonical DDR map.
		wantDynNames := make([]string, 0, len(canonicalDDRDynamicOutputs))
		for k := range canonicalDDRDynamicOutputs {
			wantDynNames = append(wantDynNames, k)
		}
		sort.Strings(wantDynNames) // lexicographic — matches the contract

		if len(dynSlice) != len(wantDynNames) {
			t.Errorf("FAIL: dynamicOutputs length = %d, want %d", len(dynSlice), len(wantDynNames))
		} else {
			for i, wantName := range wantDynNames {
				wantValue := canonicalDDRDynamicOutputs[wantName]
				entry, ok := dynSlice[i].(map[string]any)
				if !ok {
					t.Errorf("FAIL: dynamicOutputs[%d] is not an object, got %T", i, dynSlice[i])
					continue
				}
				gotName, _ := entry["name"].(string)
				gotValue, _ := entry["value"].(string)
				if gotName != wantName {
					t.Errorf("FAIL: dynamicOutputs[%d].name = %q, want %q", i, gotName, wantName)
				}
				if gotValue != wantValue {
					t.Errorf("FAIL: dynamicOutputs[%d].value = %q, want %q", i, gotValue, wantValue)
				}
			}
		}

		// --- F: flat top-level _NN_ keys (dual-emit, E10) ---
		for name, wantValue := range canonicalDDRDynamicOutputs {
			gotValue, ok := got[name].(string)
			if !ok {
				t.Errorf("FAIL: flat top-level key %q is absent or not a string", name)
				continue
			}
			if gotValue != wantValue {
				t.Errorf("FAIL: flat key %q = %q, want %q", name, gotValue, wantValue)
			}
		}

		// --- G: derived scalar fields from collection ---
		assertStringField(t, got, "template", "aws-account-hashicorp-ddr")
		assertStringField(t, got, "requestMethod", "aws-account-hashicorp-ddr")
		assertStringField(t, got, "region", "us-east-2")
		assertStringField(t, got, "datacenter", "")
		assertStringField(t, got, "cloudAccount", "ITZ")
		assertStringField(t, got, "collectionId", "69650af0758b9e41de66b6ae")

		// --- H: fixed constants (disposition table — kept as-is in Phase 1) ---
		assertStringField(t, got, "reservationpurpose-0", "Demo")
		assertStringField(t, got, "accountPool", "any")
		assertStringField(t, got, "geo", "any")
		assertStringField(t, got, "customer", "")
		assertStringField(t, got, "infrastructure", "aws")

		// --- I: opportunity and iui absent when not supplied ---
		//
		// CreateInput.Opportunity is nil / empty → `opportunity` key must be absent.
		// CreateInput.IUI is "" → `iui` key must be absent.
		if _, ok := got["opportunity"]; ok {
			t.Errorf("FAIL: `opportunity` key is present but should be absent when CreateInput.Opportunity is nil/empty")
		}
		if _, ok := got["iui"]; ok {
			t.Errorf("FAIL: `iui` key is present but should be absent when CreateInput.IUI is empty")
		}

		// --- J: description constant MUST always be present ---
		//
		// Live-validation finding (Plan 114): create.sh sends `description`; the API
		// expects it. Provider must always emit "Terraform-managed reservation".
		//
		// RED: current payload.go does not emit `description`.
		assertStringField(t, got, "description", "Terraform-managed reservation")
	})

	t.Run("empty_dynamic_outputs", func(t *testing.T) {
		// Shin F10: empty dynamic_outputs → "dynamicOutputs": [] emitted, NO flat _NN_ keys.
		// The RFC mandates the always-present empty array (not omission) for zero-output case.
		t.Parallel()

		emptyInput := techzone.CreateInput{
			Name:          "golden-empty",
			Purpose:       "Demo",
			Region:        "us-east-2",
			Datacenter:    "",
			CollectionID:  "69650af0758b9e41de66b6ae",
			Template:      "aws-account-hashicorp-ddr",
			RequestMethod: "aws-account-hashicorp-ddr",
			CloudAccount:  "ITZ",
			Start:         frozenStart,
			End:           frozenEnd,
			// User MUST be set — absent → HTTP 500 from live API.
			User: "tester@example.com",
		}

		gotBytes, err := techzone.BuildCreatePayload(canonicalPlatformRaw, nil, emptyInput)
		if err != nil {
			t.Fatalf("BuildCreatePayload returned unexpected error: %v", err)
		}

		var got map[string]any
		if err := json.Unmarshal(gotBytes, &got); err != nil {
			t.Fatalf("output is not valid JSON: %v\nbytes: %s", err, gotBytes)
		}

		// dynamicOutputs must be present and be an empty array (not absent).
		dynRaw, ok := got["dynamicOutputs"]
		if !ok {
			t.Fatalf("FAIL: `dynamicOutputs` key is absent for empty-outputs case; must be [] not omitted")
		}
		dynSlice, ok := dynRaw.([]any)
		if !ok {
			t.Fatalf("FAIL: `dynamicOutputs` is not a JSON array for empty-outputs case, got %T", dynRaw)
		}
		if len(dynSlice) != 0 {
			t.Errorf("FAIL: dynamicOutputs has %d entries, want 0 (empty array)", len(dynSlice))
		}

		// No flat _NN_ keys must be present. Scan all keys for the _NN_ pattern.
		for key := range got {
			if len(key) > 1 && key[0] == '_' {
				t.Errorf("FAIL: unexpected flat _NN_ key %q found for empty-outputs case", key)
			}
		}

		// user MUST be present even with empty dynamic_outputs (live fix).
		assertStringField(t, got, "user", "tester@example.com")

		// description MUST be present even with empty dynamic_outputs.
		assertStringField(t, got, "description", "Terraform-managed reservation")
	})

	t.Run("opportunity_emitted_as_array_when_set", func(t *testing.T) {
		// Live-validation finding (Plan 114): `opportunity` MUST be a JSON array.
		// Sending a string causes HTTP 400 "Request out of policy scope".
		// When CreateInput.Opportunity has elements, the payload must contain
		// "opportunity": ["006Ka..."] — NOT "opportunity": "006Ka...".
		t.Parallel()

		inputWithOpportunity := techzone.CreateInput{
			Name:          "golden-opportunity",
			Purpose:       "Demo",
			Region:        "us-east-2",
			Datacenter:    "",
			CollectionID:  "69650af0758b9e41de66b6ae",
			Template:      "aws-account-hashicorp-ddr",
			RequestMethod: "aws-account-hashicorp-ddr",
			CloudAccount:  "ITZ",
			Start:         frozenStart,
			End:           frozenEnd,
			User:          "tester@example.com",
			// Opportunity: a single CRM opportunity ID (real format from create.sh).
			// RED: CreateInput.Opportunity is currently `string`, not `[]string` →
			// this struct literal causes a compile error until Kou changes the type.
			Opportunity: []string{"006Ka000003kVEoIAM"},
			IUI:         "test-iui-value",
		}

		gotBytes, err := techzone.BuildCreatePayload(canonicalPlatformRaw, nil, inputWithOpportunity)
		if err != nil {
			t.Fatalf("BuildCreatePayload returned unexpected error: %v", err)
		}

		var got map[string]any
		if err := json.Unmarshal(gotBytes, &got); err != nil {
			t.Fatalf("output is not valid JSON: %v\nbytes: %s", err, gotBytes)
		}

		// opportunity MUST be a JSON array (not a string).
		// RED: current code either omits it (Opportunity=="") or emits a string.
		oppRaw, ok := got["opportunity"]
		if !ok {
			t.Fatal("FAIL: `opportunity` key is absent when CreateInput.Opportunity is non-empty")
		}
		oppSlice, ok := oppRaw.([]any)
		if !ok {
			t.Fatalf("FAIL: `opportunity` is %T (%v), want JSON array ([]any). "+
				"Live API requires an array; a string causes HTTP 400.", oppRaw, oppRaw)
		}
		if len(oppSlice) != 1 {
			t.Fatalf("FAIL: opportunity array has %d elements, want 1", len(oppSlice))
		}
		oppVal, ok := oppSlice[0].(string)
		if !ok {
			t.Fatalf("FAIL: opportunity[0] is %T, want string", oppSlice[0])
		}
		if oppVal != "006Ka000003kVEoIAM" {
			t.Errorf("FAIL: opportunity[0] = %q, want %q", oppVal, "006Ka000003kVEoIAM")
		}

		// iui MUST be present when CreateInput.IUI is non-empty.
		assertStringField(t, got, "iui", "test-iui-value")

		// user must still be present.
		assertStringField(t, got, "user", "tester@example.com")

		// description must still be present.
		assertStringField(t, got, "description", "Terraform-managed reservation")
	})

	t.Run("opportunity_omitted_when_empty_slice", func(t *testing.T) {
		// When CreateInput.Opportunity is nil or empty, `opportunity` must be absent.
		t.Parallel()

		inputNoOpportunity := techzone.CreateInput{
			Name:          "golden-no-opp",
			Purpose:       "Demo",
			Region:        "us-east-2",
			Datacenter:    "",
			CollectionID:  "69650af0758b9e41de66b6ae",
			Template:      "aws-account-hashicorp-ddr",
			RequestMethod: "aws-account-hashicorp-ddr",
			CloudAccount:  "ITZ",
			Start:         frozenStart,
			End:           frozenEnd,
			User:          "tester@example.com",
			Opportunity:   nil, // empty → omit
		}

		gotBytes, err := techzone.BuildCreatePayload(canonicalPlatformRaw, nil, inputNoOpportunity)
		if err != nil {
			t.Fatalf("BuildCreatePayload returned unexpected error: %v", err)
		}

		var got map[string]any
		if err := json.Unmarshal(gotBytes, &got); err != nil {
			t.Fatalf("output is not valid JSON: %v\nbytes: %s", err, gotBytes)
		}

		if _, ok := got["opportunity"]; ok {
			t.Errorf("FAIL: `opportunity` key is present when CreateInput.Opportunity is nil; must be omitted")
		}
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// assertStringField asserts that payload[key] exists and equals wantVal.
func assertStringField(t *testing.T, payload map[string]any, key, wantVal string) {
	t.Helper()
	raw, ok := payload[key]
	if !ok {
		t.Errorf("FAIL: key %q is absent from payload", key)
		return
	}
	got, ok := raw.(string)
	if !ok {
		t.Errorf("FAIL: key %q is not a string (got %T: %v)", key, raw, raw)
		return
	}
	if got != wantVal {
		t.Errorf("FAIL: key %q = %q, want %q", key, got, wantVal)
	}
}
