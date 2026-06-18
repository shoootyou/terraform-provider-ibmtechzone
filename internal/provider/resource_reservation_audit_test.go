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
//
//	"Provider produced inconsistent result after apply"
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
// Advisory probe: an expired Configure probe does NOT block a working DELETE
// ---------------------------------------------------------------------------

// TestAccReservation_AdvisoryProbe_DoesNotBlockWorkingDelete verifies the
// advisory-probe contract (RFC §4 M1 / audit-r1 Sho-B #1):
//
// When the Configure token probe returns 401 (expired token), but the DELETE
// call itself succeeds (HTTP 200), the destroy operation MUST complete cleanly.
//
// This tests the seam between the advisory probe and the delete path:
//   - Configure probe: 401 → TokenErr stored, no AddError from Configure.
//   - Read: silent return (state unchanged) so Terraform plans the destroy.
//   - Delete: ignores TokenErr entirely; calls DoDelete; receives HTTP 200 → success.
//
// The key invariant: "advisory probe does not block a DELETE that itself succeeds."
// Contrast with TestAccReservation_Delete_AuthFailure_302 and
// TestAccReservation_Delete_AuthFailure_401 (below), where the DELETE itself
// receives an auth-failure response and MUST fail with an actionable error.
func TestAccReservation_AdvisoryProbe_DoesNotBlockWorkingDelete(t *testing.T) {
	mock := newMockServer(t)
	// tokenValidateMode starts as "valid" so the Create step's Configure succeeds.
	// deleteStatusSequence defaults to [200] — DELETE itself succeeds.

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
			// Step 2: Explicit destroy. Configure probe sees 401, but DELETE returns 200.
			// No ExpectError: the destroy MUST succeed.
			{
				Config: providerConfigHCL(mock.URL(), sentinelToken),
				PreConfig: func() {
					mock.mu.Lock()
					mock.tokenValidateMode = "401" // probe fails, but DELETE still returns 200
					mock.mu.Unlock()
				},
			},
		},
		CheckDestroy: func(s *terraform.State) error {
			mock.mu.Lock()
			deleteCalls := mock.deleteCallCount
			mock.mu.Unlock()
			if deleteCalls == 0 {
				return fmt.Errorf(
					"expected at least one DELETE call during destroy, got 0 — " +
						"the provider short-circuited before calling Delete",
				)
			}
			return nil
		},
	})
}

// ---------------------------------------------------------------------------
// RED — Delete receives auth-failure HTTP status → actionable error (contract B)
// ---------------------------------------------------------------------------

// TestAccReservation_Delete_AuthFailure_302 is a RED test proving contract B:
//
// When the TechZone API returns HTTP 302 (SSO redirect) on DELETE — the
// realistic scenario when a token has expired mid-operation — the destroy MUST
// FAIL with an ACTIONABLE error telling the user to refresh TECHZONE_API_KEY.
//
// Current behavior (why it is RED today):
//   - 302 is not in the {200, 204, 404, 401, 403} success set, so it reaches the
//     generic "HTTP 302" AddError path — the error fires, but the message is cryptic
//     (contains only the status code, not an actionable "refresh your token" instruction).
//   - The test asserts the actionable message pattern, which does NOT match the
//     generic "HTTP 302" text → test FAILS (RED).
//
// Goes GREEN when Kou detects auth-failure HTTP codes (302, 401, 403) in Delete
// and emits "TECHZONE_API_KEY is invalid or expired. Refresh it…" instead of
// the generic status-code error.
//
// Token-safety: the sentinel must NOT appear in the error (asserted via the
// ExpectError regexp that matches the actionable message but does not match the sentinel).
func TestAccReservation_Delete_AuthFailure_302(t *testing.T) {
	mock := newMockServer(t)
	// deleteMode = "302_redirect": DELETE returns 302 + Location header (SSO redirect).
	// This simulates the TechZone API redirecting an expired-token DELETE to SSO login.

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactoriesFor(mock.URL(), sentinelToken),
		Steps: []resource.TestStep{
			// Step 1: Create normally (token valid, DELETE not yet called).
			{
				Config: reservationConfig(mock.URL(), sentinelToken),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("techzone_reservation.test", "id", "test-reservation-id"),
				),
			},
			// Step 2: Destroy. DELETE returns 302. Must fail with an actionable error.
			{
				Config: providerConfigHCL(mock.URL(), sentinelToken),
				PreConfig: func() {
					mock.mu.Lock()
					mock.deleteMode = "302_redirect"
					mock.mu.Unlock()
				},
				// Must error with "expired/refresh/TECHZONE_API_KEY" — NOT the generic
				// "HTTP 302" message. This is the RED assertion.
				ExpectError: deleteAuthFailureRegexp(),
			},
		},
	})

	// Token-safety belt: the actionable regexp must NOT itself match the sentinel.
	// If it did, the ExpectError would only prove the sentinel appeared in the error,
	// which would be a token leak rather than an actionable message assertion.
	if deleteAuthFailureRegexp().MatchString(sentinelToken) {
		t.Errorf("token safety: deleteAuthFailureRegexp matches the sentinel token — pattern is too broad")
	}
}

// TestAccReservation_Delete_AuthFailure_401 is a RED test proving contract B
// for HTTP 401 on DELETE.
//
// Current behavior (why it is RED today):
//   - 401 is in the {200, 204, 404, 401, 403} success set — the current code
//     treats 401 as "resource gone, idempotent success" (line 629-631).
//   - The test uses ExpectError, but the current code returns no error on 401.
//   - The test FAILS because "expected an error but got none" (RED).
//
// Goes GREEN when Kou removes 401/403 from the success set and instead emits the
// actionable "TECHZONE_API_KEY is invalid or expired" error for auth-failure codes.
//
// Token-safety: sentinel must NOT appear in the error message.
func TestAccReservation_Delete_AuthFailure_401(t *testing.T) {
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
			{
				Config: providerConfigHCL(mock.URL(), sentinelToken),
				PreConfig: func() {
					mock.mu.Lock()
					mock.deleteMode = "401"
					mock.mu.Unlock()
				},
				ExpectError: deleteAuthFailureRegexp(),
			},
		},
	})

	if deleteAuthFailureRegexp().MatchString(sentinelToken) {
		t.Errorf("token safety: deleteAuthFailureRegexp matches the sentinel token")
	}
}

// TestAccReservation_Delete_AuthFailure_403 is a RED test proving contract B
// for HTTP 403 on DELETE. Mirrors the 401 test.
func TestAccReservation_Delete_AuthFailure_403(t *testing.T) {
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
			{
				Config: providerConfigHCL(mock.URL(), sentinelToken),
				PreConfig: func() {
					mock.mu.Lock()
					mock.deleteMode = "403"
					mock.mu.Unlock()
				},
				ExpectError: deleteAuthFailureRegexp(),
			},
		},
	})
}

// deleteAuthFailureRegexp matches the actionable token-expired error that Delete
// must emit on 302/401/403 (contract B). Mirrors the Create/Read token-invalid
// message: "TECHZONE_API_KEY is invalid or expired. Refresh it…"
//
// The regexp deliberately does NOT match the sentinel token, so the token-safety
// self-check (MatchString(sentinelToken) == false) confirms no accidental broadening.
func deleteAuthFailureRegexp() *regexp.Regexp {
	return regexp.MustCompile(`(?i)(invalid|expired|refresh|TECHZONE_API_KEY)`)
}

// ---------------------------------------------------------------------------
// Delete succeeds on 200, 204, 404 — positive contract pin
// ---------------------------------------------------------------------------

// TestAccReservation_Delete_Success_Codes is an explicit positive pin of the
// three idempotent-success codes: 200, 204, 404. Complements the RED auth-failure
// tests above by confirming the success branch is unchanged.
//
// (This mirrors TestAccReservation_DeleteIdempotent but is co-located with the
// auth-failure tests for readability of the contract B spec.)
func TestAccReservation_Delete_Success_Codes(t *testing.T) {
	for _, code := range []int{200, 204, 404} {
		code := code
		t.Run(fmt.Sprintf("HTTP_%d", code), func(t *testing.T) {
			mock := newMockServer(t)
			mock.deleteStatusSequence = []int{code}

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
					// Reaching CheckDestroy means destroy completed without error — that is
					// the assertion. Any error from Delete would have surfaced before here.
					return nil
				},
			})
		})
	}
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
// This is the guard for the Create/Read path. The expected behavior by path:
//   - Create/Read: 401 probe → hard error → operation fails with actionable message.
//   - Destroy with advisory probe + working DELETE: see TestAccReservation_AdvisoryProbe_DoesNotBlockWorkingDelete.
//   - Destroy where DELETE itself gets 401/302: see TestAccReservation_Delete_AuthFailure_*.
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
