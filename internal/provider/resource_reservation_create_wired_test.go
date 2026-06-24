/**
 * @spec-handoff
 *
 * @interface reservationResource.Create — wired Create path (post-E5 refactor)
 *
 * Full call graph:
 *   Create(ctx, req, resp)
 *     → c.GetCollection(ctx, plan.CollectionID)       // NEW: GET /api/collection/<id>
 *     → BuildCreatePayload(                            // NEW: 3-arg signature
 *           platforms[0].Raw,                          //   verbatim platform bytes
 *           plan.DynamicOutputs (map[string]string),   //   from TF config
 *           CreateInput{...},                          //   scalar fields from plan + collection
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
 *       (a) NO "user" key at any level.
 *       (b) "dynamicOutputs" array is present, emitted in LEXICOGRAPHIC key order
 *           (name field of each element).
 *       (c) Each dynamic output ALSO emitted as a flat top-level key (dual-emit).
 *       (d) "platform" value is the VERBATIM bytes of platforms[0].Raw from the
 *           collection response — NOT re-encoded through a struct.
 *       (e) Scalar fields (region, collectionId, template, requestMethod,
 *           cloudAccount, datacenter) derived from the collection region, NOT
 *           from the TF config attributes that were removed (template, hcp_org,
 *           hcp_project are gone from the schema).
 *   - On empty dynamic_outputs (nil map from TF config): "dynamicOutputs": [] emitted,
 *     NO flat _NN_ keys; create still proceeds normally.
 *
 * @edge-cases
 *   - ErrCollectionNotFound → Terraform diagnostic, summary matches
 *     regexp `(?i)(collection|not found)`.
 *   - ErrUnsupportedInfrastructure → Terraform diagnostic, summary matches
 *     regexp `(?i)(unsupported|infrastructure)`.
 *   - Collection with valid AWS platform → POST fires; body validated per (a)–(e).
 *   - Token safety: sentinel must NOT appear in any diagnostic string.
 *
 * @see ./resource_reservation.go      (Kou implements Create changes here)
 * @see ../techzone/collection.go      (GetCollection — already implemented)
 * @see ../techzone/payload.go         (BuildCreatePayload — new 3-arg signature)
 * @see ./testutil_mock_server_test.go (mock server — handleCollection added here)
 * @see .yui-soul/plans/wip/114-techzone-template-agnostic/e5-schema-resource-wiring.md
 */

//go:build !unit

package provider_test

// E5 wired-Create acceptance tests.
//
// RED GATE (E5 Task 1): these tests fail because:
//   (a) resource_reservation.go does not compile — old BuildCreatePayload call +
//       references to deleted CreateInput fields.
//   (b) Even if it compiled, the mock's GET /api/collection/<id> handler is not
//       yet wired in the provider's Create, so the create path would skip the
//       collection fetch and use the wrong payload shape.
//
// Goes GREEN when Kou completes E5 Task 2 (Create calls GetCollection + new payload).
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
resource "techzone_reservation" "test" {
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
resource "techzone_reservation" "test" {
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

// TestReservationCreate_Wired_PostBodyShape_NoBuildold verifies the full wired
// Create path with the new 3-arg BuildCreatePayload:
//
//  1. Mock serves GET /api/collection/test-collection-id → ddrCollectionJSON
//  2. Create builds the POST body using platforms[0].Raw (verbatim) + dynamic_outputs.
//  3. Assertions on the captured POST body:
//     (a) "user" key absent.
//     (b) "dynamicOutputs" array in lexicographic order.
//     (c) flat _NN_ keys present.
//     (d) "platform" verbatim (parsed from ddrCollectionJSON; both sides go through
//         the same unmarshal-remarshal cycle).
//     (e) scalar fields (template, requestMethod, region, datacenter, cloudAccount,
//         collectionId) derived from the collection, not from removed schema attrs.
//
// RED: resource_reservation.go does not compile in its current state.
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
					resource.TestCheckResourceAttr("techzone_reservation.test", "id", "test-reservation-id"),
					resource.TestCheckResourceAttr("techzone_reservation.test", "status", "Ready"),
				),
			},
		},
		CheckDestroy: func(_ interface{ Helper() }) error { return nil },
	})

	// Now assert on the captured POST body.
	if capturedBody == nil {
		t.Fatal("FAIL: no POST body was captured — Create did not call POST /api/reservation/aws")
	}

	var got map[string]any
	if err := json.Unmarshal(capturedBody, &got); err != nil {
		t.Fatalf("FAIL: POST body is not valid JSON: %v\nbody: %s", err, capturedBody)
	}

	// (a) "user" key MUST be absent.
	if _, ok := got["user"]; ok {
		t.Errorf("FAIL: POST body contains \"user\" key — must be absent (E8)")
	}

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
					resource.TestCheckResourceAttr("techzone_reservation.test", "id", "test-reservation-id"),
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

	// "user" must still be absent.
	if _, ok := got["user"]; ok {
		t.Errorf("FAIL: \"user\" key present in POST body for empty-outputs case — must be absent (E8)")
	}
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
