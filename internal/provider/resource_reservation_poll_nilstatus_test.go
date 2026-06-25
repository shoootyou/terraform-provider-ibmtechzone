/**
 * @spec-handoff — Audit Remediation (Round 1) — poll nil-status retry
 *
 * @interface mockTechZoneServer — pollResponseSequence capability (new field)
 * @interface reservationResource.Create — poll loop nil-status handling
 *
 * @behavior
 *   - mockTechZoneServer gains a new field `pollResponseSequence []string` and
 *     method `SetPollResponseSequence(seq []string)`.
 *   - When pollResponseSequence is set (non-nil), handlePoll uses it:
 *       - For the nth poll call (0-indexed), returns pollResponseSequence[n].
 *       - When the index exceeds the slice length, repeats the last element.
 *   - When pollResponseSequence is nil (default), handlePoll uses the existing
 *     readyAfterPollCount/pollShouldFail logic (backwards compatible).
 *   - An empty string "" in the sequence represents a nil-status body: `{}`.
 *   - A non-empty string is used verbatim as the poll response body.
 *
 *   POLL LOOP CONTRACT (resource_reservation.go:443-450):
 *   - A poll response with Status == nil (body lacks "status" key, or "status": null)
 *     MUST trigger: tflog.Warn + continue (retry on next tick).
 *   - After retrying, if the next response has Status = "Ready", Create MUST succeed.
 *   - The poll loop MUST NOT return an error or panic on a nil-status response.
 *
 * @contracts-for-kou
 *   testutil_mock_server_test.go:
 *     1. Add field: `pollResponseSequence []string`
 *     2. Add method: SetPollResponseSequence(seq []string) — sets the sequence under mu.
 *     3. Modify handlePoll: if m.pollResponseSequence != nil, use sequence-based response.
 *        An empty string in the sequence returns `{}` (nil-status body).
 *        Otherwise return the string verbatim as JSON body.
 *
 *   resource_reservation.go — NO CHANGE NEEDED (nil-status guard already exists at L443-450).
 *
 * @see ./testutil_mock_server_test.go
 * @see ./resource_reservation.go:443-450
 * @see ../../.yui-soul/reviews/114-techzone-template-agnostic/r1-shin-tests.md (F-02)
 * @see ../../.yui-soul/reviews/114-techzone-template-agnostic/r1-sho-provider.md
 */

//go:build !unit

// Package provider_test — poll nil-status retry acceptance test.
//
// RED GATE: This test FAILS TO COMPILE because:
//   (a) mockTechZoneServer has no SetPollResponseSequence method.
//       The call mock.SetPollResponseSequence(...) will produce a compile error:
//       "mock.SetPollResponseSequence undefined (type *mockTechZoneServer has no
//       field or method SetPollResponseSequence)"
//
// Goes GREEN when Kou adds:
//   - pollResponseSequence field + SetPollResponseSequence method to mockTechZoneServer
//   - handlePoll logic to use the sequence when set
//   (The nil-status guard in resource_reservation.go already exists and is correct.)
//
// Run with:
//   TF_ACC=1 go test ./internal/provider/ -run TestAccReservation_PollWithNilStatus -v -timeout 5m
package provider_test

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccReservation_PollWithNilStatus_RetriesAndSucceeds verifies that the poll
// loop correctly handles a nil/missing status in a poll response.
//
// Sequence:
//   poll 0: 200 + `{}` (no "status" key → Status == nil → warning + continue)
//   poll 1: 200 + `{"status":"Ready"}` → loop exits, Create proceeds to canonical GET
//
// Expected outcome: Create succeeds, id = "test-reservation-id".
//
// RED: mock.SetPollResponseSequence is not yet defined → compile error.
func TestAccReservation_PollWithNilStatus_RetriesAndSucceeds(t *testing.T) {
	mock := newMockServer(t)

	// Set up the poll response sequence:
	//   - First poll: empty body (nil status) → should trigger warn+continue
	//   - Second poll: Ready → loop exits
	//
	// RED: SetPollResponseSequence is undefined on *mockTechZoneServer until Kou adds it.
	mock.SetPollResponseSequence([]string{
		"",                    // poll 0: {} → Status == nil → continue
		`{"status":"Ready"}`,  // poll 1: Ready → exit loop
	})

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactoriesFor(mock.URL(), sentinelToken),
		Steps: []resource.TestStep{
			{
				Config: reservationConfig(mock.URL(), sentinelToken),
				Check: resource.ComposeAggregateTestCheckFunc(
					// Create must succeed despite the first poll returning nil status.
					resource.TestCheckResourceAttr("ibmtechzone_reservation.test", "id", "test-reservation-id"),
					resource.TestCheckResourceAttr("ibmtechzone_reservation.test", "status", "Ready"),
				),
			},
		},
	})

	// Verify that the mock received at least 2 poll calls (nil-status + Ready).
	mock.mu.Lock()
	pollCount := mock.pollCount
	mock.mu.Unlock()

	if pollCount < 2 {
		t.Errorf(
			"FAIL: poll loop should have been called at least 2 times (nil-status + Ready), "+
				"got %d poll calls\n"+
				"  This suggests the nil-status path is not retrying correctly.",
			pollCount,
		)
	}
}

// TestAccReservation_PollWithNilStatus_NullStatusField verifies that a poll response
// with `"status": null` (explicit JSON null) is also treated as nil-status and retried.
//
// RED: same compile error as above (SetPollResponseSequence undefined).
func TestAccReservation_PollNullStatusField_RetriesAndSucceeds(t *testing.T) {
	mock := newMockServer(t)

	// Sequence: explicit null status field → then Ready.
	mock.SetPollResponseSequence([]string{
		`{"status": null}`,    // poll 0: explicit null → Status *string == nil → continue
		`{"status":"Ready"}`,  // poll 1: Ready
	})

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactoriesFor(mock.URL(), sentinelToken),
		Steps: []resource.TestStep{
			{
				Config: reservationConfig(mock.URL(), sentinelToken),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("ibmtechzone_reservation.test", "id", "test-reservation-id"),
					resource.TestCheckResourceAttr("ibmtechzone_reservation.test", "status", "Ready"),
				),
			},
		},
	})
}
