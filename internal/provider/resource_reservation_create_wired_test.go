/**
 * @spec-handoff
 *
 * @interface reservationResource.Create — wired Create path (post-Plan-114 live fix)
 *
 * Full call graph:
 *   Create(ctx, req, resp)
 *     → c.GetCollection(ctx, plan.CollectionID)       // GET /api/collection/<id>
 *     → BuildCreatePayload(                            // 3-arg signature
 *           platforms[0].Raw,                          //   verbatim platform bytes
 *           plan.DynamicOutputs (map[string]string),   //   from TF config
 *           CreateInput{                               //   scalar fields
 *             User:        plan.UserEmail,             //   REQUIRED — absent → HTTP 500
 *             Opportunity: plan.RequesterContext.opportunity ([]string, when set),
 *             IUI:         plan.RequesterContext.iui   (string, when set),
 *             ...                                      //   collection-derived fields
 *           },
 *       )
 *     → DoPost(ctx, "/api/reservation/aws", payload)   // unchanged
 *     → poll loop                                      // unchanged
 *     → final GET + state write                        // unchanged
 *
 * @behavior
 *   - Calls GET /api/collection/<id> BEFORE building the create payload.
 *   - On ErrCollectionNotFound from GetCollection: AddError with summary containing
 *     "collection" and detail containing "not found" (or equivalent); no POST fired.
 *   - On ErrUnsupportedInfrastructure from GetCollection: AddError with summary
 *     containing "unsupported" or "infrastructure"; no POST fired.
 *   - POST /api/reservation/aws body MUST satisfy ALL of:
 *       (a) "user" key PRESENT and equal to plan.UserEmail (user_email TF attribute).
 *           Live API returns HTTP 500 "Invalid user assignment" when absent.
 *       (b) "dynamicOutputs" array is present, emitted in LEXICOGRAPHIC key order
 *           (name field of each element).
 *       (c) Each dynamic output ALSO emitted as a flat top-level key (dual-emit).
 *       (d) "platform" value is the VERBATIM bytes of platforms[0].Raw from the
 *           collection response — NOT re-encoded through a struct.
 *       (e) Scalar fields (region, collectionId, template, requestMethod,
 *           cloudAccount, datacenter) derived from the collection region.
 *       (f) "description" = "Terraform-managed reservation" always present.
 *       (g) "opportunity" emitted as a JSON ARRAY when requester_context.opportunity
 *           is set; OMITTED when requester_context is absent/null.
 *       (h) "iui" emitted as a string when requester_context.iui is set and non-empty;
 *           OMITTED otherwise.
 *   - On empty dynamic_outputs (nil map from TF config): "dynamicOutputs": [] emitted,
 *     NO flat _NN_ keys; create still proceeds normally.
 *
 * @edge-cases
 *   - ErrCollectionNotFound → Terraform diagnostic, summary matches
 *     regexp `(?i)(collection|not found)`.
 *   - ErrUnsupportedInfrastructure → Terraform diagnostic, summary matches
 *     regexp `(?i)(unsupported|infrastructure)`.
 *   - Collection with valid AWS platform → POST fires; body validated per (a)–(h).
 *   - Token safety: sentinel must NOT appear in any diagnostic string.
 *   - requester_context absent from HCL → "opportunity" and "iui" absent from POST body.
 *   - requester_context present with opportunity list → "opportunity" in POST body is
 *     a JSON array, NOT a string.
 *
 * @see ./resource_reservation.go      (Kou implements Create changes here)
 * @see ../techzone/collection.go      (GetCollection — already implemented)
 * @see ../techzone/payload.go         (BuildCreatePayload — updated CreateInput)
 * @see ./testutil_mock_server_test.go (mock server)
 * @see .yui-soul/plans/wip/114-techzone-template-agnostic/e5-schema-resource-wiring.md
 */

//go:build !unit

package provider_test

// Wired-Create acceptance tests (E5 + Plan 114 live-validation fix).
//
// RED GATE (Plan 114 live fix): existing tests now also FAIL because:
//   (a) CreateInput.User does not exist / Opportunity is string not []string →
//       package compile error in payload.go / payload_golden_test.go.
//   (b) Even if it compiled, the POST body would be missing "user" → assertion (a)
//       in TestReservationCreate_Wired_PostBodyShape now expects user PRESENT.
//   (c) "description" is absent from POST body → new assertion (f) fails.
//   (d) TestReservationCreate_Wired_WithRequesterContext (NEW) requires
//       requester_context in schema + wired Create → RED until Kou implements.
//
// Goes GREEN when Kou:
//   1. Adds User + []string Opportunity to CreateInput in payload.go.
//   2. Adds requester_context to Schema() and RequesterContext to reservationModel.
//   3. Wires user ← user_email and requester_context fields into CreateInput in Create().
//
// Run with:
//   TF_ACC=1 go test ./internal/provider/ -run TestReservation -v -timeout 5m

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// ---------------------------------------------------------------------------
// DDR collection fixture — mirrors collection_test.go but as an HTTP response
// ---------------------------------------------------------------------------

// ddrCollectionJSON is a minimal DDR-shaped collection response for the mock server.
// platforms[0].infrastructure == "aws"; regions[0] has the DDR template fields.
//
// NOTE: key order is intentionally non-alphabetical ("oid" before "id") in the
// platform element to enable byte-verbatim assertion (same technique as payload golden test).
const ddrCollectionJSON = `{
  "id": "test-collection-id",
  "platforms": [
    {
      "oid": "62ccb18c2d38520017eec9fb",
      "id": "69651c138d6e497dc77a8dbe",
      "name": "Reservation Name",
      "infrastructure": "aws",
      "regions": [
        {
          "name": "US East 2",
          "region": "us-east-2",
          "datacenter": "",
          "template": "aws-account-hashicorp-ddr",
          "requestMethod": "aws-account-hashicorp-ddr",
          "cloudAccount": "ITZ",
          "pattern": {
            "id": "ccp-gitops/aws-account-hashicorp-ddr/itz",
            "name": "aws-account-hashicorp-ddr",
            "profile": "itz"
          }
        }
      ]
    }
  ]
}`

// vmwareCollectionJSON triggers ErrUnsupportedInfrastructure.
const vmwareCollectionJSON = `{
  "id": "test-collection-id",
  "platforms": [
    {
      "id": "vmware-platform-001",
      "name": "VMware Lab",
      "infrastructure": "vmware",
      "regions": [
        {
          "name": "DC1",
          "region": "dc1",
          "datacenter": "dc-west",
          "template": "vmware-tmpl",
          "requestMethod": "vmware-tmpl",
          "cloudAccount": "ACME",
          "pattern": {"id": "x", "name": "x", "profile": "x"}
        }
      ]
    }
  ]
}`

// ---------------------------------------------------------------------------
// Terraform HCL configs — new schema (no template/hcp_org/hcp_project;
// dynamic_outputs map present)
// ---------------------------------------------------------------------------

// reservationConfigV2 is the E5-era replacement for reservationConfig.
// It omits template/hcp_org/hcp_project and populates dynamic_outputs.
func reservationConfigV2(mockURL, apiKey string) string {
	return providerConfigHCL(mockURL, apiKey) + `
resource "ibmtechzone_reservation" "test" {
  collection_id             = "test-collection-id"
  user_email                = "test@example.com"
  dynamic_outputs           = {
    "_04_hcp_org"     = "test-hcp-org"
    "_05_hcp_project" = "test-hcp-project"
  }
  timeout_minutes           = 1
  reservation_duration_days = 1
}
`
}

// reservationConfigV2_EmptyOutputs uses an empty dynamic_outputs map.
func reservationConfigV2_EmptyOutputs(mockURL, apiKey string) string {
	return providerConfigHCL(mockURL, apiKey) + `
resource "ibmtechzone_reservation" "test" {
  collection_id             = "test-collection-id"
  user_email                = "test@example.com"
  dynamic_outputs           = {}
  timeout_minutes           = 1
  reservation_duration_days = 1
}
`
}

// ---------------------------------------------------------------------------
// Scenario: CREATE — happy path with DDR collection
// ---------------------------------------------------------------------------

// TestReservationCreate_Wired_PostBodyShape verifies the full wired
// Create path with the updated BuildCreatePayload:
//
//  1. Mock serves GET /api/collection/test-collection-id → ddrCollectionJSON
//  2. Create builds the POST body using platforms[0].Raw (verbatim) + dynamic_outputs.
//  3. Assertions on the captured POST body:
//     (a) "user" key PRESENT and equal to user_email ("test@example.com").
//         Plan 114 live fix: absent `user` → HTTP 500 from real API.
//     (b) "dynamicOutputs" array in lexicographic order.
//     (c) flat _NN_ keys present.
//     (d) "platform" verbatim (parsed from ddrCollectionJSON; both sides go through
//         the same unmarshal-remarshal cycle).
//     (e) scalar fields (template, requestMethod, region, datacenter, cloudAccount,
//         collectionId) derived from the collection.
//     (f) "description" = "Terraform-managed reservation" present (Plan 114 live fix).
//
// RED: CreateInput.User doesn't exist / Opportunity is string → compile error.
// Also RED: user absent from POST body → assertion (a) fails.
func TestReservationCreate_Wired_PostBodyShape(t *testing.T) {
	mock := newMockServer(t)
	mock.SetCollectionResponse("test-collection-id", 200, ddrCollectionJSON)

	var capturedBody []byte
	mock.SetCreateBodyCapture(func(body []byte) { capturedBody = body })

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactoriesFor(mock.URL(), sentinelToken),
		Steps: []resource.TestStep{
			{
				Config: reservationConfigV2(mock.URL(), sentinelToken),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("ibmtechzone_reservation.test", "id", "test-reservation-id"),
					resource.TestCheckResourceAttr("ibmtechzone_reservation.test", "status", "Ready"),
				),
			},
		},
			CheckDestroy: nil,
	})

	// Now assert on the captured POST body.
	if capturedBody == nil {
		t.Fatal("FAIL: no POST body was captured — Create did not call POST /api/reservation/aws")
	}

	var got map[string]any
	if err := json.Unmarshal(capturedBody, &got); err != nil {
		t.Fatalf("FAIL: POST body is not valid JSON: %v\nbody: %s", err, capturedBody)
	}

	// (a) "user" key MUST be present and equal to user_email.
	// Plan 114 live fix: live API returns HTTP 500 "Invalid user assignment" when absent.
	// E8 decision was wrong — server uses submitted value for myId assignment.
	//
	// RED: current Create() does not wire user_email → CreateInput.User → payload.
	assertBodyString(t, got, "user", "test@example.com")

	// (b) "dynamicOutputs" array in lexicographic order.
	dynRaw, ok := got["dynamicOutputs"]
	if !ok {
		t.Fatal("FAIL: POST body missing \"dynamicOutputs\" key")
	}
	dynSlice, ok := dynRaw.([]any)
	if !ok {
		t.Fatalf("FAIL: \"dynamicOutputs\" is %T, want []any", dynRaw)
	}
	wantDynKeys := []string{"_04_hcp_org", "_05_hcp_project"}
	sort.Strings(wantDynKeys) // ensure test expectation is also lex-sorted
	if len(dynSlice) != len(wantDynKeys) {
		t.Errorf("FAIL: dynamicOutputs has %d entries, want %d", len(dynSlice), len(wantDynKeys))
	} else {
		for i, wantKey := range wantDynKeys {
			entry, ok := dynSlice[i].(map[string]any)
			if !ok {
				t.Errorf("FAIL: dynamicOutputs[%d] is %T, want map", i, dynSlice[i])
				continue
			}
			gotName, _ := entry["name"].(string)
			if gotName != wantKey {
				t.Errorf("FAIL: dynamicOutputs[%d].name = %q, want %q (lexicographic order)", i, gotName, wantKey)
			}
		}
	}

	// (c) Flat _NN_ keys present as top-level string values.
	wantFlat := map[string]string{
		"_04_hcp_org":     "test-hcp-org",
		"_05_hcp_project": "test-hcp-project",
	}
	for k, wantV := range wantFlat {
		gotV, ok := got[k].(string)
		if !ok {
			t.Errorf("FAIL: flat key %q absent or not a string in POST body", k)
			continue
		}
		if gotV != wantV {
			t.Errorf("FAIL: flat key %q = %q, want %q", k, gotV, wantV)
		}
	}

	// (d) "platform" verbatim — parse both sides through unmarshal-remarshal cycle.
	platformVal, ok := got["platform"]
	if !ok {
		t.Fatal("FAIL: POST body missing \"platform\" key")
	}
	gotPlatformBytes, err := json.Marshal(platformVal)
	if err != nil {
		t.Fatalf("FAIL: cannot re-marshal platform value: %v", err)
	}
	// Extract platforms[0] from the ddrCollectionJSON fixture and apply the same
	// unmarshal-remarshal cycle for a fair byte-comparison baseline.
	var collEnvelope struct {
		Platforms []json.RawMessage `json:"platforms"`
	}
	if err := json.Unmarshal([]byte(ddrCollectionJSON), &collEnvelope); err != nil {
		t.Fatalf("FAIL: cannot unmarshal ddrCollectionJSON: %v", err)
	}
	if len(collEnvelope.Platforms) == 0 {
		t.Fatal("FAIL: ddrCollectionJSON has no platforms")
	}
	var wantPlatformParsed any
	if err := json.Unmarshal(collEnvelope.Platforms[0], &wantPlatformParsed); err != nil {
		t.Fatalf("FAIL: cannot unmarshal platform[0]: %v", err)
	}
	wantPlatformBytes, err := json.Marshal(wantPlatformParsed)
	if err != nil {
		t.Fatalf("FAIL: cannot re-marshal want platform: %v", err)
	}
	if string(gotPlatformBytes) != string(wantPlatformBytes) {
		t.Errorf("FAIL: platform bytes mismatch\n  got:  %s\n  want: %s",
			gotPlatformBytes, wantPlatformBytes)
	}

	// (e) Scalar fields derived from collection region.
	assertBodyString(t, got, "template", "aws-account-hashicorp-ddr")
	assertBodyString(t, got, "requestMethod", "aws-account-hashicorp-ddr")
	assertBodyString(t, got, "region", "us-east-2")
	assertBodyString(t, got, "datacenter", "")
	assertBodyString(t, got, "cloudAccount", "ITZ")
	assertBodyString(t, got, "collectionId", "test-collection-id")

	// (f) description constant always present.
	// RED: current payload.go does not emit `description`.
	assertBodyString(t, got, "description", "Terraform-managed reservation")
}

// ---------------------------------------------------------------------------
// Scenario: CREATE — empty dynamic_outputs
// ---------------------------------------------------------------------------

// TestReservationCreate_Wired_EmptyDynamicOutputs verifies that an empty
// dynamic_outputs map results in "dynamicOutputs": [] and NO flat _NN_ keys.
//
// RED: same compile-fail as TestReservationCreate_Wired_PostBodyShape.
func TestReservationCreate_Wired_EmptyDynamicOutputs(t *testing.T) {
	mock := newMockServer(t)
	mock.SetCollectionResponse("test-collection-id", 200, ddrCollectionJSON)

	var capturedBody []byte
	mock.SetCreateBodyCapture(func(body []byte) { capturedBody = body })

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactoriesFor(mock.URL(), sentinelToken),
		Steps: []resource.TestStep{
			{
				Config: reservationConfigV2_EmptyOutputs(mock.URL(), sentinelToken),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("ibmtechzone_reservation.test", "id", "test-reservation-id"),
				),
			},
		},
	})

	if capturedBody == nil {
		t.Fatal("FAIL: no POST body captured")
	}

	var got map[string]any
	if err := json.Unmarshal(capturedBody, &got); err != nil {
		t.Fatalf("FAIL: POST body not valid JSON: %v", err)
	}

	// dynamicOutputs must be an empty array.
	dynRaw, ok := got["dynamicOutputs"]
	if !ok {
		t.Fatal("FAIL: POST body missing \"dynamicOutputs\" key for empty-outputs case")
	}
	dynSlice, ok := dynRaw.([]any)
	if !ok {
		t.Fatalf("FAIL: \"dynamicOutputs\" is %T, want []any", dynRaw)
	}
	if len(dynSlice) != 0 {
		t.Errorf("FAIL: dynamicOutputs has %d entries, want 0 (empty array)", len(dynSlice))
	}

	// No flat _NN_ keys.
	for key := range got {
		if len(key) > 1 && key[0] == '_' {
			t.Errorf("FAIL: unexpected flat _NN_ key %q in POST body for empty-outputs case", key)
		}
	}

	// "user" MUST be present even with empty dynamic_outputs (Plan 114 live fix).
	// RED: current Create() does not wire user_email into payload.
	assertBodyString(t, got, "user", "test@example.com")

	// "description" constant must be present regardless of dynamic_outputs.
	assertBodyString(t, got, "description", "Terraform-managed reservation")
}

// ---------------------------------------------------------------------------
// Scenario: CREATE — requester_context wired into POST body (Plan 114 live fix)
// ---------------------------------------------------------------------------

// reservationConfigV2_WithRequesterContext returns a TF config that sets
// requester_context with opportunity + iui, exercising the new schema attribute.
func reservationConfigV2_WithRequesterContext(mockURL, apiKey string) string {
	return providerConfigHCL(mockURL, apiKey) + `
resource "ibmtechzone_reservation" "test" {
  collection_id             = "test-collection-id"
  user_email                = "test@example.com"
  dynamic_outputs           = {
    "_04_hcp_org" = "test-hcp-org"
  }
  timeout_minutes           = 1
  reservation_duration_days = 1
  requester_context = {
    opportunity = ["006Ka000003kVEoIAM"]
    iui         = "test-iui-42"
  }
}
`
}

// TestReservationCreate_Wired_WithRequesterContext verifies that when
// requester_context is set in the TF config:
//   (a) "opportunity" in the POST body is a JSON ARRAY (not a string).
//       Live API returns HTTP 400 when opportunity is a string.
//   (b) "iui" in the POST body is the string value from requester_context.iui.
//   (c) "user" is still present and equals user_email.
//
// RED:
//   - `requester_context` does not exist in schema → config parsing error.
//   - CreateInput.User / []string Opportunity don't exist → compile error.
//   - Create() does not wire requester_context → opportunity absent from body.
func TestReservationCreate_Wired_WithRequesterContext(t *testing.T) {
	mock := newMockServer(t)
	mock.SetCollectionResponse("test-collection-id", 200, ddrCollectionJSON)

	var capturedBody []byte
	mock.SetCreateBodyCapture(func(body []byte) { capturedBody = body })

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactoriesFor(mock.URL(), sentinelToken),
		Steps: []resource.TestStep{
			{
				Config: reservationConfigV2_WithRequesterContext(mock.URL(), sentinelToken),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("ibmtechzone_reservation.test", "id", "test-reservation-id"),
					resource.TestCheckResourceAttr("ibmtechzone_reservation.test", "status", "Ready"),
				),
			},
		},
	})

	if capturedBody == nil {
		t.Fatal("FAIL: no POST body captured — Create did not call POST /api/reservation/aws")
	}

	var got map[string]any
	if err := json.Unmarshal(capturedBody, &got); err != nil {
		t.Fatalf("FAIL: POST body is not valid JSON: %v\nbody: %s", err, capturedBody)
	}

	// (a) "opportunity" must be a JSON array, NOT a string.
	// RED: current code either omits opportunity or emits a string.
	oppRaw, ok := got["opportunity"]
	if !ok {
		t.Fatal("FAIL: POST body missing \"opportunity\" key when requester_context.opportunity is set")
	}
	oppSlice, ok := oppRaw.([]any)
	if !ok {
		t.Fatalf("FAIL: \"opportunity\" is %T (%v), want []any (JSON array). "+
			"Live API requires array; string → HTTP 400.", oppRaw, oppRaw)
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

	// (b) "iui" must be the string from requester_context.iui.
	assertBodyString(t, got, "iui", "test-iui-42")

	// (c) "user" must be present.
	assertBodyString(t, got, "user", "test@example.com")

	// description constant still present.
	assertBodyString(t, got, "description", "Terraform-managed reservation")
}

// TestReservationCreate_Wired_NoRequesterContext verifies that when
// requester_context is absent from the TF config, "opportunity" and "iui"
// are both absent from the POST body (not emitted as null or empty).
//
// RED: same compile-fail as above.
func TestReservationCreate_Wired_NoRequesterContext(t *testing.T) {
	mock := newMockServer(t)
	mock.SetCollectionResponse("test-collection-id", 200, ddrCollectionJSON)

	var capturedBody []byte
	mock.SetCreateBodyCapture(func(body []byte) { capturedBody = body })

	// reservationConfigV2 does NOT set requester_context.
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactoriesFor(mock.URL(), sentinelToken),
		Steps: []resource.TestStep{
			{
				Config: reservationConfigV2(mock.URL(), sentinelToken),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("ibmtechzone_reservation.test", "id", "test-reservation-id"),
				),
			},
		},
	})

	if capturedBody == nil {
		t.Fatal("FAIL: no POST body captured")
	}

	var got map[string]any
	if err := json.Unmarshal(capturedBody, &got); err != nil {
		t.Fatalf("FAIL: POST body is not valid JSON: %v", err)
	}

	// opportunity and iui must be absent when requester_context is not set.
	if _, ok := got["opportunity"]; ok {
		t.Errorf("FAIL: \"opportunity\" present in POST body when requester_context absent; must be omitted")
	}
	if _, ok := got["iui"]; ok {
		t.Errorf("FAIL: \"iui\" present in POST body when requester_context absent; must be omitted")
	}

	// user must still be present.
	assertBodyString(t, got, "user", "test@example.com")
}

// ---------------------------------------------------------------------------
// Scenario: CREATE — collection not found → TF diagnostic
// ---------------------------------------------------------------------------

// TestReservationCreate_CollectionNotFound_Diagnostic verifies that when
// GET /api/collection/<id> returns 404, Create surfaces a Terraform diagnostic
// error containing "collection" or "not found", and does NOT fire POST.
//
// RED: same compile-fail. Additionally, current Create does not call GetCollection
// at all, so even if it compiled the ExpectError would not match.
func TestReservationCreate_CollectionNotFound_Diagnostic(t *testing.T) {
	mock := newMockServer(t)
	mock.SetCollectionResponse("test-collection-id", 404, `{"error":"not found"}`)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactoriesFor(mock.URL(), sentinelToken),
		Steps: []resource.TestStep{
			{
				Config:      reservationConfigV2(mock.URL(), sentinelToken),
				ExpectError: regexp.MustCompile(`(?i)(collection|not found)`),
			},
		},
	})

	// POST must NOT have been called.
	mock.mu.Lock()
	createCallCount := mock.createCallCount
	mock.mu.Unlock()
	if createCallCount > 0 {
		t.Errorf("FAIL: POST /api/reservation/aws was called %d time(s) after collection 404; want 0", createCallCount)
	}

	// Token safety.
	mock.mu.Lock()
	headers := mock.AuthHeaders
	mock.mu.Unlock()
	for _, hdr := range headers {
		if strings.Contains(hdr, sentinelToken) && !strings.HasPrefix(hdr, "Bearer ") {
			t.Errorf("token safety: Authorization header is not Bearer format: %q", hdr)
		}
	}
}

// ---------------------------------------------------------------------------
// Scenario: CREATE — unsupported infrastructure → TF diagnostic
// ---------------------------------------------------------------------------

// TestReservationCreate_UnsupportedInfrastructure_Diagnostic verifies that when
// GET /api/collection/<id> returns a vmware collection, Create surfaces a
// Terraform diagnostic containing "unsupported" or "infrastructure".
//
// RED: same compile-fail + Create does not call GetCollection.
func TestReservationCreate_UnsupportedInfrastructure_Diagnostic(t *testing.T) {
	mock := newMockServer(t)
	mock.SetCollectionResponse("test-collection-id", 200, vmwareCollectionJSON)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactoriesFor(mock.URL(), sentinelToken),
		Steps: []resource.TestStep{
			{
				Config:      reservationConfigV2(mock.URL(), sentinelToken),
				ExpectError: regexp.MustCompile(`(?i)(unsupported|infrastructure)`),
			},
		},
	})

	// POST must NOT have been called.
	mock.mu.Lock()
	createCallCount := mock.createCallCount
	mock.mu.Unlock()
	if createCallCount > 0 {
		t.Errorf("FAIL: POST was called %d time(s) after unsupported-infra error; want 0", createCallCount)
	}
}

// ---------------------------------------------------------------------------
// Scenario: Schema — new config (no template/hcp_org/hcp_project) is valid
// ---------------------------------------------------------------------------

// TestReservationSchema_V2Config_IsValid asserts that the new HCL config shape
// (dynamic_outputs map, no template/hcp_org/hcp_project) is accepted by the
// schema without diagnostics.
//
// RED: the current schema still has template/hcp_org/hcp_project as Required and
// does not have dynamic_outputs. The new config will produce schema errors.
func TestReservationSchema_V2Config_IsValid(t *testing.T) {
	mock := newMockServer(t)
	mock.SetCollectionResponse("test-collection-id", 200, ddrCollectionJSON)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactoriesFor(mock.URL(), sentinelToken),
		Steps: []resource.TestStep{
			{
				// If the schema is correct, this should be a valid plan (no errors).
				// The create itself may fail (poll timeout in unit mode), but the
				// schema validation must not add any diagnostics.
				Config:             reservationConfigV2(mock.URL(), sentinelToken),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true, // resource does not exist yet → non-empty plan
			},
		},
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// assertBodyString is a local helper (mirrors assertStringField from payload_golden_test.go)
// for asserting a string field in the decoded POST body.
func assertBodyString(t *testing.T, body map[string]any, key, want string) {
	t.Helper()
	raw, ok := body[key]
	if !ok {
		t.Errorf("FAIL: POST body missing key %q", key)
		return
	}
	got, ok := raw.(string)
	if !ok {
		t.Errorf("FAIL: POST body key %q is %T (%v), want string", key, raw, raw)
		return
	}
	if got != want {
		t.Errorf("FAIL: POST body key %q = %q, want %q", key, got, want)
	}
}
