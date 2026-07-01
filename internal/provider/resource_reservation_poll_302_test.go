/**
 * @spec-handoff
 *
 * @interface reservationResource.Create — poll loop endpoint
 *
 * @behavior
 *   - The poll loop during Create MUST use GET /api/reservation/aws/<id>
 *     (the "typed" endpoint that returns status directly with no redirect).
 *   - The poll loop MUST NOT use GET /api/reservation/<id> (the "legacy"
 *     endpoint that permanently redirects to /api/reservation/unknown/<id>).
 *   - The provider's HTTP client does NOT follow redirects
 *     (CheckRedirect = http.ErrUseLastResponse — intentional for SSO-redirect
 *     detection). A 302 from the legacy endpoint is therefore observed raw and
 *     classified as "non-2xx, retry" by the poll loop.
 *   - Because the 302 is permanent (legacy endpoint always redirects), the poll
 *     loop must never reach a "Ready" status when it uses the legacy endpoint —
 *     the loop will exhaust maxAttempts and time out.
 *   - When the poll loop correctly uses /api/reservation/aws/<id>, the mock
 *     returns 200+Ready and the Create succeeds.
 *
 * @edge-cases
 *   - Legacy endpoint (/api/reservation/<id>) returns 302 → poll loop retries
 *     forever → timeout diagnostic ("did not reach Ready within N minutes").
 *   - Typed endpoint (/api/reservation/aws/<id>) returns 200+Ready → poll loop
 *     exits, Create succeeds, state is populated with status=Ready.
 *   - PollURLLog (mock assertion field) must contain ONLY paths matching
 *     /api/reservation/aws/<id>, never /api/reservation/<id> (excluding the
 *     /aws/ sub-path).
 *
 * @see ./resource_reservation.go:491  (current bug: polls /api/reservation/<id>)
 * @see ./resource_reservation.go:569  (correct canonical GET: /api/reservation/aws/<id>)
 * @see ./testutil_mock_server_test.go (pollLegacyMode, PollURLLog, awsPollResponseSequence)
 * @see .yui-soul/plans/wip/114-techzone-template-agnostic/  (active plan)
 *
 * REGRESSION TARGET:
 *   Production hang reproduced in: terraform apply (reservation POST ok, poll hangs).
 *   Root cause: poll loop at resource_reservation.go:491 uses /api/reservation/<id>
 *   (legacy), which returns HTTP 302. Client sees 302 (does not follow), poll loop
 *   treats it as non-2xx and retries forever → "Still creating…" until timeout.
 *   Fix: change poll endpoint to /api/reservation/aws/<id>.
 */

//go:build !unit

// Package provider_test — poll 302 regression test (RED gate).
//
// RED GATE: TestAccReservation_Poll_LegacyEndpointReturns302_MustNotHang
//
// Current code polls /api/reservation/<id> (legacy).
// Mock returns HTTP 302 on that path (permanent redirect — mimics real API).
// Provider client does NOT follow redirects → sees 302 raw → "non-2xx, retry".
// Poll loop exhausts maxAttempts → times out → Terraform error:
//   "Reservation test-reservation-id did not reach Ready within 1 minutes."
//
// The test EXPECTS the apply to SUCCEED (status=Ready). It fails (RED) because
// the current code gets only 302s and times out.
//
// Goes GREEN when Kou changes the poll path from:
//   /api/reservation/<id>
// to:
//   /api/reservation/aws/<id>
// (resource_reservation.go:491)
//
// Run with:
//   TF_ACC=1 go test ./internal/provider/ -run TestAccReservation_Poll_LegacyEndpointReturns302 -v -timeout 5m
package provider_test

import (
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccReservation_Poll_LegacyEndpointReturns302_MustNotHang is the RED
// regression test for the production poll-hang bug.
//
// Setup:
//   - GET /api/reservation/<id>      → HTTP 302 (mock: pollLegacyMode="302_redirect")
//     Mimics real TechZone API: legacy endpoint always redirects to
//     /api/reservation/unknown/<id>. The provider client receives 302 raw.
//   - GET /api/reservation/aws/<id>  → 200 + {"status":"Ready"} (mock default)
//     The typed endpoint returns status directly — what the fix will use.
//
// Expected (GREEN — what the fixed code does):
//   The poll loop uses /api/reservation/aws/<id>, receives Ready, apply succeeds.
//   PollURLLog contains only /api/reservation/aws/test-reservation-id paths.
//
// Actual (RED — what the current code does):
//   The poll loop uses /api/reservation/<id>, receives 302 every time,
//   retries until maxAttempts (timeout_minutes=1 → 6 attempts), then errors:
//   "Reservation test-reservation-id did not reach Ready within 1 minutes."
//   apply returns a Terraform error — the test's Check (id=test-reservation-id)
//   is never reached.
//
// Timing: timeout_minutes=1 → maxAttempts=6 (6 × 10s = 60s worst case).
// In the RED state, the poll loop fires 6 times: attempt 0 is immediate, then
// 5 × 10s waits = ~50s total. The go test -timeout is 5m so this is safe.
func TestAccReservation_Poll_LegacyEndpointReturns302_MustNotHang(t *testing.T) {
	mock := newMockServer(t)

	// Configure the legacy poll path to always return 302 (permanent redirect).
	// This is the mock representation of the real TechZone API regression:
	// GET /api/reservation/<id> → 302 → Location: /api/reservation/unknown/<id>
	//
	// The typed path /api/reservation/aws/<id> is NOT given special treatment here;
	// it uses the mock default (handleCanonicalRead → "ready" mode → 200+Ready).
	// When the fix is in place, the poll loop will hit /aws/<id> and exit immediately.
	mock.mu.Lock()
	mock.pollLegacyMode = "302_redirect"
	mock.mu.Unlock()

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactoriesFor(mock.URL(), sentinelToken),
		Steps: []resource.TestStep{
			{
				// timeout_minutes=1 → maxAttempts=6 → caps test duration at ~60s.
				// The test MUST succeed (status=Ready) — proving the poll loop
				// never hit the 302-returning legacy endpoint.
				//
				// RED: current code polls /api/reservation/<id> → 302 every time
				// → "did not reach Ready within 1 minutes." → step errors before
				// Check is ever evaluated.
				Config: reservationConfig(mock.URL(), sentinelToken),
				Check: resource.ComposeAggregateTestCheckFunc(
					// If we reach here, the poll succeeded — the fix is in place.
					resource.TestCheckResourceAttr("ibmtechzone_reservation.test", "id", "test-reservation-id"),
					resource.TestCheckResourceAttr("ibmtechzone_reservation.test", "status", "Ready"),
				),
			},
		},
	})

	// --- URL-path assertions (belt-and-suspenders) ---
	// After a green run, the poll loop must have used /api/reservation/aws/<id>,
	// NOT the legacy /api/reservation/<id>.
	//
	// In the RED state these assertions are unreachable (the apply step above errors),
	// so they only run when the fix is in place (and must pass).
	mock.mu.Lock()
	pollLog := make([]string, len(mock.PollURLLog))
	copy(pollLog, mock.PollURLLog)
	mock.mu.Unlock()

	for _, path := range pollLog {
		// Any path matching /api/reservation/<id> (without /aws/) is the legacy
		// endpoint. The poll loop must NEVER use it.
		if strings.HasPrefix(path, "/api/reservation/") &&
			!strings.HasPrefix(path, "/api/reservation/aws/") &&
			path != "/api/reservation/aws" {
			t.Errorf(
				"FAIL: poll loop used legacy endpoint %q — must use /api/reservation/aws/<id>.\n"+
					"  This is the endpoint that returns HTTP 302 on the real TechZone API,\n"+
					"  causing the poll loop to retry forever (production hang).",
				path,
			)
		}
	}

	// Verify at least one poll request hit the typed endpoint.
	var awsPollSeen bool
	for _, path := range pollLog {
		if strings.HasPrefix(path, "/api/reservation/aws/") {
			awsPollSeen = true
			break
		}
	}
	// Guard is unconditional: the apply step above already asserted status=Ready,
	// which requires at least one successful poll. An empty pollLog after a green
	// apply would mean the mock's PollURLLog was never populated — itself a bug.
	// Making this unconditional removes the vacuous-skip risk: `len(pollLog) > 0`
	// could silently pass if the log were somehow empty (Shin F-02).
	if !awsPollSeen {
		t.Errorf(
			"FAIL: no poll requests hit /api/reservation/aws/<id> — "+
				"poll log: %v.\n"+
				"  The fix must route the poll loop to the typed endpoint.",
			pollLog,
		)
	}
}
