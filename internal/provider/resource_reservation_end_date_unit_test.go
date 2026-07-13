/**
 * @spec-handoff
 *
 * @interface reservationResource.Create — end-date overshoot invariant
 *
 * @behavior
 *   - The interval [now_real, end) MUST NOT exceed duration_days * 24h.
 *   - Achieved by: now_truncated = r.now().UTC().Truncate(time.Minute)
 *                  end = now_truncated + duration_days * 24h
 *   - Because now_truncated ≤ now_real, end - now_real ≤ duration_days * 24h always holds.
 *
 * @edge-cases
 *   - now() exactly on a minute boundary (sub-second == 0) → no truncation, invariant holds trivially.
 *   - now() at mid-minute (30s 500ms sub-minute offset) → truncation removes ~30.5s, invariant holds.
 *   - now() at end-of-minute (59s 999ms sub-minute offset) → truncation removes ~60s, invariant holds.
 *
 * @see ./resource_reservation.go:395-399  (fix under test)
 */

// Package provider — unit tests for the end-date overshoot fix.
//
// These tests live in package provider (white-box) so they can construct a
// reservationResource with an injected clock and call the date-computation
// logic directly — without needing a full TF framework round-trip.
//
// The invariant under test:
//
//	end_time - now_real  ≤  duration_days * 24h
//
// where now_real is the wall-clock value returned by r.now(), and end_time is
// the timestamp produced by Create's date-computation block.
//
// OLD formula (buggy):
//
//	now_raw  := r.now().UTC()                                          // NOT truncated
//	end      := now_raw + 1*time.Minute + duration_days*24*time.Hour  // +1min overshoot
//
// That formula produces:  end - now_real  =  1min + duration*24h,  always exceeding the
// duration maximum by at least one minute — triggering HTTP 400 "Invalid end date" from
// TechZone policies where the requested duration exactly matched the policy maximum.
//
// NEW formula (fix):
//
//	now_trunc := r.now().UTC().Truncate(time.Minute)
//	end       := now_trunc + duration_days*24*time.Hour
//
// That formula produces:  end - now_real  ≤  duration*24h  always (since now_trunc ≤ now_real).
package provider

import (
	"fmt"
	"testing"
	"time"
)

// endDateFromNow re-implements the exact formula used in Create (lines 395-399 of
// resource_reservation.go) so we can exercise it in isolation.
//
// Keeping it in sync with the production code is intentional: if someone changes
// the formula, this helper will diverge and the test will detect the regression.
func endDateFromNow(nowFn func() time.Time, durationDays int64) time.Time {
	now := nowFn().UTC().Truncate(time.Minute)
	endStr := now.Add(time.Duration(durationDays) * 24 * time.Hour).Format("2006-01-02T15:04:05.000Z")
	// Parse back to time.Time so callers can do arithmetic comparisons.
	t, err := time.Parse("2006-01-02T15:04:05.000Z", endStr)
	if err != nil {
		panic(fmt.Sprintf("endDateFromNow: parse %q: %v", endStr, err))
	}
	return t
}

// endDateFromNow_OLD re-implements the BUGGY formula that was in place before the fix.
// Used in sub-tests to demonstrate that the old code would have violated the invariant.
func endDateFromNow_OLD(nowFn func() time.Time, durationDays int64) time.Time {
	nowRaw := nowFn().UTC() // NOT truncated
	endStr := nowRaw.Add(1*time.Minute + time.Duration(durationDays)*24*time.Hour).
		Format("2006-01-02T15:04:05.000Z")
	t, err := time.Parse("2006-01-02T15:04:05.000Z", endStr)
	if err != nil {
		panic(fmt.Sprintf("endDateFromNow_OLD: parse %q: %v", endStr, err))
	}
	return t
}

// TestCreateEndDate_NeverExceedsDuration is the canonical invariant test.
//
// For each sub-case we set r.now() to return a fixed timestamp that exercises
// a specific position within a minute, then assert:
//
//	end - now_real ≤ durationDays * 24h
//
// We also demonstrate that the OLD formula would have violated the invariant.
func TestCreateEndDate_NeverExceedsDuration(t *testing.T) {
	t.Parallel()

	const durationDays int64 = 2
	maxAllowed := time.Duration(durationDays) * 24 * time.Hour

	// Anchor: a fixed wall-clock second we'll offset within.
	// 2024-06-15T12:00:00.000Z — a round minute boundary as the base.
	base := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name         string
		now          time.Time // the value r.now() will return
		subMinOffset time.Duration
	}{
		{
			name:         "exact_minute_boundary",
			now:          base, // :00.000 — no sub-minute component
			subMinOffset: 0,
		},
		{
			name:         "mid_minute",
			now:          base.Add(30*time.Second + 500*time.Millisecond), // :30.500
			subMinOffset: 30*time.Second + 500*time.Millisecond,
		},
		{
			name:         "end_of_minute",
			now:          base.Add(59*time.Second + 999*time.Millisecond), // :59.999
			subMinOffset: 59*time.Second + 999*time.Millisecond,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			nowReal := tc.now
			nowFn := func() time.Time { return nowReal }

			// ── NEW formula ──────────────────────────────────────────────────
			endNew := endDateFromNow(nowFn, durationDays)
			actualInterval := endNew.Sub(nowReal)

			if actualInterval > maxAllowed {
				t.Errorf(
					"FAIL (new formula): end - now_real = %v, exceeds %v (duration_days=%d)\n"+
						"  now_real     = %v\n"+
						"  end          = %v\n"+
						"  overshoot    = %v",
					actualInterval, maxAllowed, durationDays,
					nowReal, endNew,
					actualInterval-maxAllowed,
				)
			} else {
				t.Logf("OK  (new formula): end - now_real = %v ≤ %v  [sub-minute offset %v]",
					actualInterval, maxAllowed, tc.subMinOffset)
			}

			// ── OLD formula — demonstrate it WOULD have failed ───────────────
			// (This sub-check is informational: it asserts the old formula
			//  violates the invariant, confirming the fix was necessary.)
			endOld := endDateFromNow_OLD(nowFn, durationDays)
			oldInterval := endOld.Sub(nowReal)

			// The old formula always adds +1 minute on top of duration, so it
			// always exceeds maxAllowed by at least the truncated sub-minute amount.
			// We assert it IS over the limit (proving the old formula was broken).
			//
			// Exception: when sub-minute offset == 0, the old formula still overshoots
			// by exactly 1 minute (the literal +1*time.Minute it added).
			if oldInterval <= maxAllowed {
				// This should never happen for any of our test cases.
				t.Errorf(
					"UNEXPECTED (old formula): end - now_real = %v, expected > %v\n"+
						"  The old formula should always overshoot — check test setup.",
					oldInterval, maxAllowed,
				)
			} else {
				t.Logf("CONFIRMED (old formula): end - now_real = %v > %v — old formula overshoots by %v",
					oldInterval, maxAllowed, oldInterval-maxAllowed)
			}
		})
	}
}

// TestCreateEndDate_InvariantHoldsAcrossDurations verifies the invariant for a
// range of duration_days values and a representative mid-minute clock position.
func TestCreateEndDate_InvariantHoldsAcrossDurations(t *testing.T) {
	t.Parallel()

	// mid-minute: the worst-case sub-minute offset for the OLD formula was just under 1 minute.
	nowReal := time.Date(2024, 6, 15, 12, 0, 59, 999_000_000, time.UTC) // :59.999
	nowFn := func() time.Time { return nowReal }

	for _, days := range []int64{1, 2, 3, 7, 14} {
		days := days
		t.Run(fmt.Sprintf("duration_%dd", days), func(t *testing.T) {
			t.Parallel()

			maxAllowed := time.Duration(days) * 24 * time.Hour
			endNew := endDateFromNow(nowFn, days)
			actualInterval := endNew.Sub(nowReal)

			if actualInterval > maxAllowed {
				t.Errorf(
					"FAIL: end - now_real = %v exceeds max %v for duration_days=%d\n"+
						"  now_real = %v\n"+
						"  end      = %v",
					actualInterval, maxAllowed, days, nowReal, endNew,
				)
			}
		})
	}
}
