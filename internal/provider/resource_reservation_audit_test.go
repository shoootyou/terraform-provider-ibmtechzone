// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

//go:build !unit

package provider_test

// Audit round-1 regression tests — HIGH findings (Sho-A #1, Shin F-2/Sho-B #1).
//
// RED tests (MUST fail against current code):
//   TestAccReservation_InPlaceUpdateOperational — proves the UseStateForUnknown bug
//   TestAccReservation_DestroyWithExpiredToken  — proves the advisory-probe bug
//
// Coverage additions (GREEN against current code):
//   TestAccReservation_ReadExpiredStatusPrune   — Expired terminal-status acc arm
//   TestAccReservation_DeleteBodyShape          — asserts DELETE request body fields
//   TestAccTokenValidation_ExpiredToken_Create  — replaces the incorrectly-scoped
//                                                 Scenario 8 with a Create-path guard

import (
	"encoding/json"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// ---------------------------------------------------------------------------
// RED #1 — In-place Update regression (Sho-A #1)
// ---------------------------------------------------------------------------

// TestAccReservation_InPlaceUpdateOperational is a RED test that proves the
// HIGH finding Sho-A #1: changing ONLY timeout_minutes (an operational, non-
// RequiresReplace attribute) on an existing reservation should produce a clean
// in-place Update with no replacement and no error.
//
// Current failure mode:
//   "Provider produced inconsistent result after apply"
//
// Root cause: status, start_date, and end_date are Computed attributes with no
// UseStateForUnknown() plan modifier.  On an operational-only Update the Framework
// marks them as "(known after apply)" in the plan.  The Update method copies state
// to state correctly, but the Framework then sees the plan said "unknown" while the
// new state carries concrete values → inconsistency error.
//
// Goes GREEN when Kou adds UseStateForUnknown() to status, start_date, and end_date.
func TestAccReservation_InPlaceUpdateOperational(t *testing.T) {
	mock := newMockServer(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactoriesFor(mock.URL(), sentinelToken),
		Steps: []resource.TestStep{
			// Step 1: Create the reservation.
			{
				Config: reservationConfigWithTimeout(mock.URL(), sentinelToken, 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("techzone_reservation.test", "id", "test-reservation-id"),
					resource.TestCheckResourceAttr("techzone_reservation.test", "status", "Ready"),
					resource.TestCheckResourceAttr("techzone_reservation.test", "timeout_minutes", "1"),
				),
			},
			// Step 2: Change ONLY timeout_minutes from 1 to 2.
			// This is an operational-only change — must NOT replace the resource.
			// The id and status must remain stable.
			// Today this step emits "Provider produced inconsistent result after apply".
			{
				Config: reservationConfigWithTimeout(mock.URL(), sentinelToken, 2),
				Check: resource.ComposeAggregateTestCheckFunc(
					// id must be stable — no replacement.
					resource.TestCheckResourceAttr("techzone_reservation.test", "id", "test-reservation-id"),
					// status must be preserved from state — no refresh, no API call.
					resource.TestCheckResourceAttr("techzone_reservation.test", "status", "Ready"),
					// timeout_minutes must reflect the new value.
					resource.TestCheckResourceAttr("techzone_reservation.test", "timeout_minutes", "2"),
					// start_date and end_date must be stable (not "(known after apply)").
					resource.TestCheckResourceAttrSet("techzone_reservation.test", "start_date"),
					resource.TestCheckResourceAttrSet("techzone_reservation.test", "end_date"),
				),
			},
		},
	})
}

// reservationConfigWithTimeout returns a config identical to reservationConfig
// but with an explicit timeout_minutes value so we can change it between steps.
func reservationConfigWithTimeout(mockURL, apiKey string, timeoutMinutes int) string {
	return fmt.Sprintf(`
provider "techzone" {
  api_key  = %q
  api_base = %q
}

resource "techzone_reservation" "test" {
  collection_id             = "test-collection-id"
  user_email                = "test@example.com"
  hcp_org                   = "test-hcp-org"
  hcp_project               = "test-hcp-project"
  timeout_minutes           = %d
  reservation_duration_days = 1
}
`, apiKey, mockURL, timeoutMinutes)
}

// ---------------------------------------------------------------------------
// RED #2 — Destroy with expired token succeeds (Shin F-2 / Sho-B #1)
// ---------------------------------------------------------------------------

// TestAccReservation_DestroyWithExpiredToken is a RED test that proves the
// HIGH finding Sho-B #1 / RFC §6 scenario 8 (corrected):
// terraform destroy of an existing reservation MUST succeed even when the
// token-validate probe returns 401.
//
// Current failure mode:
//   Configure returns a hard error on 401 → the destroy graph is aborted
//   before Delete is ever called.
//
// Root cause: provider.Configure does not distinguish between create/read paths
// (where the token check is load-bearing) and the destroy path (where it is not —
// RFC §3.3/§4 M1: "Delete MUST tolerate an auth failure").
//
// Goes GREEN when Kou makes the Configure probe advisory on destroy (e.g. skip or
// downgrade to warning when the operation context indicates a destroy).
//
// Note: TestAccTokenValidation_ExpiredToken_Create (below) preserves the Create/Read
// path guard for the 401 case.
func TestAccReservation_DestroyWithExpiredToken(t *testing.T) {
	mock := newMockServer(t)
	// tokenValidateMode starts as "valid" so the Create step's Configure succeeds.

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactoriesFor(mock.URL(), sentinelToken),
		Steps: []resource.TestStep{
			// Step 1: Create normally with a valid token.
			{
				Config: reservationConfig(mock.URL(), sentinelToken),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("techzone_reservation.test", "id", "test-reservation-id"),
				),
			},
			// Step 2: Explicit destroy step.
			// Switch the token-validate endpoint to 401 immediately before
			// Terraform destroys — the PreConfig hook runs before the step's
			// plan/apply/destroy phase, so Configure will see 401 during destroy.
			// The destroy MUST succeed (DELETE is still called and returns 200)
			// despite the Configure probe returning 401.
			{
				// Empty config causes Terraform to plan a destroy of all existing resources.
				Config: providerConfigHCL(mock.URL(), sentinelToken),
				PreConfig: func() {
					// Switch to 401 so the destroy-phase Configure probe fails.
					mock.mu.Lock()
					mock.tokenValidateMode = "401"
					mock.mu.Unlock()
				},
				// No ExpectError: the destroy must succeed despite the 401 probe.
				// If Configure hard-errors, this step will emit an error and the
				// test FAILS (RED) — that's the expected current behavior.
			},
		},
		// After the explicit destroy step completes, assert the DELETE was called.
		CheckDestroy: func(s *terraform.State) error {
			mock.mu.Lock()
			deleteCalls := mock.deleteCallCount
			mock.mu.Unlock()
			if deleteCalls == 0 {
				return fmt.Errorf(
					"expected at least one DELETE call during destroy, got 0 — " +
						"the provider may have short-circuited destroy due to the Configure 401 error",
				)
			}
			return nil
		},
	})
}

// ---------------------------------------------------------------------------
// Coverage addition #5 — "Expired" terminal-status acceptance arm (Shin F-5)
// ---------------------------------------------------------------------------

// TestAccReservation_ReadExpiredStatusPrune verifies the "Expired" branch of the
// terminal-status prune path: GET /api/reservation/aws/<id> returns 200 + status="Expired".
// This exercises the IsTerminalStatus("Expired") == true path through the full
// provider Read → RemoveResource() chain (unit tests cover the logic;
// this test covers the wiring through the provider's Read method).
func TestAccReservation_ReadExpiredStatusPrune(t *testing.T) {
	mock := newMockServer(t)
	// Add "expired" mode to the mock (mirrors "deleted" but with status="Expired").

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactoriesFor(mock.URL(), sentinelToken),
		Steps: []resource.TestStep{
			// Step 1: Create.
			{
				Config: reservationConfig(mock.URL(), sentinelToken),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("techzone_reservation.test", "id", "test-reservation-id"),
				),
			},
			// Step 2: Switch to "expired" canonical read → provider prunes → non-empty plan.
			{
				PreConfig: func() {
					mock.mu.Lock()
					mock.canonicalReadMode = "expired"
					mock.mu.Unlock()
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

// ---------------------------------------------------------------------------
// Coverage addition #6 — Assert the Delete request body shape (Shin F-6)
// ---------------------------------------------------------------------------

// TestAccReservation_DeleteBodyShape verifies the shape of the DELETE request body
// sent by the provider: must be {"IBMID": <user_email>, "requestType": "aws",
// "reservationId": <id>} — mirroring delete.sh's hardcoded payload (RFC §3.4).
//
// PII guard: user_email must NOT appear in any error string (Ei F-07), but the
// delete body itself is intentional — we assert the field is present, not leaked.
func TestAccReservation_DeleteBodyShape(t *testing.T) {
	mock := newMockServer(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactoriesFor(mock.URL(), sentinelToken),
		Steps: []resource.TestStep{
			{
				Config: reservationConfig(mock.URL(), sentinelToken),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("techzone_reservation.test", "id", "test-reservation-id"),
				),
			},
		},
		CheckDestroy: func(s *terraform.State) error {
			mock.mu.Lock()
			bodies := mock.DeleteBodies
			mock.mu.Unlock()

			if len(bodies) == 0 {
				return fmt.Errorf("expected at least one DELETE call, got 0")
			}

			// Parse the first (and only) DELETE body.
			var payload map[string]string
			if err := json.Unmarshal(bodies[0], &payload); err != nil {
				return fmt.Errorf("DELETE body is not valid JSON: %v (body: %q)", err, bodies[0])
			}

			// Assert required fields.
			if payload["IBMID"] != "test@example.com" {
				return fmt.Errorf("DELETE body: IBMID = %q, want %q", payload["IBMID"], "test@example.com")
			}
			if payload["requestType"] != "aws" {
				return fmt.Errorf("DELETE body: requestType = %q, want %q", payload["requestType"], "aws")
			}
			if payload["reservationId"] != "test-reservation-id" {
				return fmt.Errorf("DELETE body: reservationId = %q, want %q", payload["reservationId"], "test-reservation-id")
			}
			return nil
		},
	})
}

// ---------------------------------------------------------------------------
// Coverage addition — Token validation failure on Create/Read path (replaces
// old Scenario 8 which incorrectly tested the destroy path)
// ---------------------------------------------------------------------------

// TestAccTokenValidation_ExpiredToken_Create verifies that a 401 on the token-
// validate probe during a CREATE-path operation surfaces the actionable
// "TECHZONE_API_KEY is invalid or expired" error.
//
// This is the guard for the Create/Read path (as opposed to
// TestAccReservation_DestroyWithExpiredToken which covers the destroy path).
// Both paths must be covered but their expected behavior differs:
//   - Create/Read: 401 → hard error → operation fails with actionable message.
//   - Destroy:     401 → advisory (or skipped) → operation succeeds.
func TestAccTokenValidation_ExpiredToken_Create(t *testing.T) {
	mock := newMockServer(t)
	mock.tokenValidateMode = "401"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactoriesFor(mock.URL(), sentinelToken),
		Steps: []resource.TestStep{
			{
				// Resource block forces Configure to run on the create path.
				Config:      reservationConfig(mock.URL(), sentinelToken),
				ExpectError: regexp.MustCompile(`(?i)(invalid|expired|refresh|TECHZONE_API_KEY)`),
			},
		},
	})
}
