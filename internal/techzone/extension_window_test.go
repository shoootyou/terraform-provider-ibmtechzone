// @spec-handoff
//
// @interface InExtensionWindow(provisionUntil string, now time.Time, durationDays int64, windowFraction float64) (eligible bool, ok bool)
//
// @behavior
//   - durationDays <= 0 → (false, false): "cannot determine" — distinct from
//     "not eligible" so the caller (Read()) can log this instead of silently
//     treating it as the routine not-yet-eligible outcome.
//   - provisionUntil unparseable via ToEpoch → (false, false).
//   - Otherwise: windowSeconds = int64(windowFraction * float64(durationDays) * 86400)
//     (truncated to int64 seconds); windowStart = ToEpoch(provisionUntil) - windowSeconds;
//     eligible = now.Unix() >= windowStart (inclusive lower bound, no upper bound).
//     Returns (eligible, true).
//   - No upper bound: once now is at/after windowStart, eligible stays true no
//     matter how far past provisionUntil now advances.
//   - Never calls time.Now() internally — now is always caller-injected (mirrors
//     PastExpiry's existing contract).
//   - windowFraction is caller-supplied; this function has no opinion on its
//     validity bounds (that is the schema validator's job, §3d of the spec).
//
// @edge-cases
//   - durationDays == 0            → (false, false)
//   - durationDays == -1 (negative) → (false, false)
//   - provisionUntil == ""          → (false, false)
//   - provisionUntil == "not-a-date" → (false, false)
//   - provisionUntil == "null"      → (false, false)
//   - Pinned (default fraction 0.5, durationDays=4, provisionUntil="2026-07-20T00:00:00Z",
//     epoch 1784505600): windowStart epoch 1784332800 ("2026-07-18T00:00:00Z").
//     now == windowStart           → (true, true)   // inclusive lower bound
//     now == windowStart - 1s      → (false, true)
//     now == windowStart + 1s      → (true, true)
//   - Pinned (non-default fraction 0.25, same provisionUntil/durationDays):
//     windowStart epoch 1784419200 ("2026-07-19T00:00:00Z").
//     now == windowStart           → (true, true)
//     now == windowStart - 1s      → (false, true)
//   - Space-separated regression: provisionUntil = "2026-07-20 00:00:00" (no "T",
//     no tz marker — TechZone's real wire format) at the same now values as the
//     default-fraction RFC3339 case above MUST produce identical (eligible, ok)
//     pairs — proves InExtensionWindow delegates correctly to the now-fixed
//     ToEpoch space-separated branch.
//   - fraction == 0.0 (degenerate, not rejected by the validator — §3d row 4):
//     windowStart == provisionUntil epoch itself. now == provisionUntil epoch → true;
//     now == provisionUntil epoch - 1s → false.
//   - No-upper-bound: now 100 days after provisionUntil (default fraction) → (true, true).
//
// @interface NextExtensionDate(provisionUntil string, durationDays int64) (string, bool)
//
// @behavior
//   - durationDays <= 0 → ("", false).
//   - provisionUntil unparseable via ToEpoch → ("", false).
//   - Otherwise: next = ToEpoch(provisionUntil) + durationDays*86400 seconds,
//     formatted as "2006-01-02T15:04:05.000Z" (UTC) — the identical layout
//     Create() already uses for start/end. Returns (formatted, true).
//
// @edge-cases
//   - durationDays == 0             → ("", false)
//   - durationDays == -1 (negative) → ("", false)
//   - provisionUntil == ""          → ("", false)
//   - provisionUntil == "not-a-date" → ("", false)
//   - Pinned: NextExtensionDate("2026-07-20T00:00:00Z", 4) → "2026-07-24T00:00:00.000Z"
//     (epoch 1784851200).
//   - Space-separated regression: NextExtensionDate("2026-07-20 00:00:00", 4) →
//     the identical "2026-07-24T00:00:00.000Z" — proves the space-separated
//     wire format is handled identically to RFC3339 for the increment path too.
//   - Invariant across several durationDays values (1, 4, 7, 30): re-parsing the
//     returned string via ToEpoch and subtracting the original ToEpoch(provisionUntil)
//     yields exactly durationDays*86400 seconds.
//
// @interface DefaultExtensionWindowFraction (const, float64)
//
// @behavior
//   - Equals exactly 0.5. This is the single source of truth wired into the
//     ibmtechzone_reservation resource's extension_window_fraction schema Default
//     (float64default.StaticFloat64(DefaultExtensionWindowFraction) in
//     resource_reservation.go). A silent change here changes the shipped default
//     for every resource that does not override it — pinned to catch that.
//
// @see ./extension_window.go (Kou implements in E6)
// @see ./expiry.go (ToEpoch — the parsing primitive both functions above build on)
// @see .yui-soul/plans/wip/04-expiry-fix-and-extension-window/e4-extension-window-spec.md §1/§2
// @see .yui-soul/knowledge/gotchas/ibm-techzone.md (fail-silent-to-KEEP finding — the
//   (false,false)/("",false) "cannot determine" contracts above are the deliberate
//   mitigation: distinct from "not eligible" so callers must observe, not swallow.)

package techzone_test

import (
	"testing"
	"time"

	"github.com/shoootyou-ext/terraform-provider-ibmtechzone/internal/techzone"
)

// ---------------------------------------------------------------------------
// TestDefaultExtensionWindowFraction — pin the exported default constant
// ---------------------------------------------------------------------------

func TestDefaultExtensionWindowFraction(t *testing.T) {
	t.Parallel()
	const want = 0.5
	if techzone.DefaultExtensionWindowFraction != want {
		t.Errorf("DefaultExtensionWindowFraction = %v, want %v (this is the schema Default "+
			"single source of truth — a silent change here changes the shipped default for "+
			"every resource that doesn't override extension_window_fraction)",
			techzone.DefaultExtensionWindowFraction, want)
	}
}

// ---------------------------------------------------------------------------
// TestInExtensionWindow
// ---------------------------------------------------------------------------

func TestInExtensionWindow(t *testing.T) {
	t.Parallel()

	// Pinned anchor (spec §1): provisionUntil = "2026-07-20T00:00:00Z", epoch 1784505600.
	const provisionUntilISO = "2026-07-20T00:00:00Z"
	const provisionUntilSpace = "2026-07-20 00:00:00" // same instant, TechZone's real wire format

	// Independently verified via Go's own time package (never hand-calculated —
	// plan README Decision D5): epoch(provisionUntilISO) = 1784505600.
	provisionUntilEpoch := int64(1784505600)

	type row struct {
		name           string
		provisionUntil string
		now            time.Time
		durationDays   int64
		windowFraction float64
		wantEligible   bool
		wantOK         bool
	}

	rows := []row{
		// --- durationDays <= 0: "cannot determine", regardless of anything else ---
		{
			name:           "durationDays_zero_cannotDetermine",
			provisionUntil: provisionUntilISO,
			now:            time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC),
			durationDays:   0,
			windowFraction: 0.5,
			wantEligible:   false,
			wantOK:         false,
		},
		{
			name:           "durationDays_negative_cannotDetermine",
			provisionUntil: provisionUntilISO,
			now:            time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC),
			durationDays:   -1,
			windowFraction: 0.5,
			wantEligible:   false,
			wantOK:         false,
		},

		// --- provisionUntil unparseable: "cannot determine" ---
		{
			name:           "provisionUntil_empty_cannotDetermine",
			provisionUntil: "",
			now:            time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC),
			durationDays:   4,
			windowFraction: 0.5,
			wantEligible:   false,
			wantOK:         false,
		},
		{
			name:           "provisionUntil_garbage_cannotDetermine",
			provisionUntil: "not-a-date",
			now:            time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC),
			durationDays:   4,
			windowFraction: 0.5,
			wantEligible:   false,
			wantOK:         false,
		},
		{
			name:           "provisionUntil_literal_null_cannotDetermine",
			provisionUntil: "null",
			now:            time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC),
			durationDays:   4,
			windowFraction: 0.5,
			wantEligible:   false,
			wantOK:         false,
		},

		// --- default fraction (0.5), durationDays=4: windowStart epoch 1784332800
		// ("2026-07-18T00:00:00Z") — pinned, spec §1 ---
		{
			name:           "default_fraction_at_windowStart_eligible",
			provisionUntil: provisionUntilISO,
			now:            time.Unix(provisionUntilEpoch-172800, 0).UTC(), // == windowStart
			durationDays:   4,
			windowFraction: 0.5,
			wantEligible:   true,
			wantOK:         true,
		},
		{
			name:           "default_fraction_1s_before_windowStart_notEligible",
			provisionUntil: provisionUntilISO,
			now:            time.Unix(provisionUntilEpoch-172800-1, 0).UTC(), // windowStart - 1s
			durationDays:   4,
			windowFraction: 0.5,
			wantEligible:   false,
			wantOK:         true,
		},
		{
			name:           "default_fraction_1s_after_windowStart_eligible",
			provisionUntil: provisionUntilISO,
			now:            time.Unix(provisionUntilEpoch-172800+1, 0).UTC(), // windowStart + 1s
			durationDays:   4,
			windowFraction: 0.5,
			wantEligible:   true,
			wantOK:         true,
		},

		// --- non-default fraction (0.25), durationDays=4: windowStart epoch
		// 1784419200 ("2026-07-19T00:00:00Z") — pinned, spec §1 ---
		{
			name:           "nondefault_fraction_at_windowStart_eligible",
			provisionUntil: provisionUntilISO,
			now:            time.Unix(provisionUntilEpoch-86400, 0).UTC(), // == windowStart (0.25)
			durationDays:   4,
			windowFraction: 0.25,
			wantEligible:   true,
			wantOK:         true,
		},
		{
			name:           "nondefault_fraction_1s_before_windowStart_notEligible",
			provisionUntil: provisionUntilISO,
			now:            time.Unix(provisionUntilEpoch-86400-1, 0).UTC(), // windowStart(0.25) - 1s
			durationDays:   4,
			windowFraction: 0.25,
			wantEligible:   false,
			wantOK:         true,
		},

		// --- fraction=0.3 (audit round-1 finding #4 — float truncation):
		// 0.3 is not exactly representable in binary float64, so
		// windowFraction*durationDays*86400 lands a hair below the exact
		// integer (25919.999999999996, not 25920.0, for durationDays=1).
		// Truncating (the pre-fix behavior) silently drops that fraction of
		// a second, moving windowStart ONE SECOND LATER than the contract's
		// inclusive-lower-bound requires — at exactly this boundary, the
		// pre-fix code incorrectly returned eligible=false. Fixed via
		// math.Round. windowStart epoch independently verified via Go's own
		// math.Round + time package in this environment (never
		// hand-calculated, per plan README Decision D5):
		// provisionUntilEpoch(1784505600) - round(0.3*1*86400) (round(25919.999999999996)=25920)
		// == 1784479680 ("2026-07-19T16:48:00Z"). ---
		{
			name:           "fraction_0.3_float_rounding_at_windowStart_eligible",
			provisionUntil: provisionUntilISO,
			now:            time.Unix(provisionUntilEpoch-25920, 0).UTC(), // == correctly-rounded windowStart
			durationDays:   1,
			windowFraction: 0.3,
			wantEligible:   true,
			wantOK:         true,
		},
		{
			name:           "fraction_0.3_float_rounding_1s_before_windowStart_notEligible",
			provisionUntil: provisionUntilISO,
			now:            time.Unix(provisionUntilEpoch-25920-1, 0).UTC(),
			durationDays:   1,
			windowFraction: 0.3,
			wantEligible:   false,
			wantOK:         true,
		},

		// --- space-separated regression: identical (eligible, ok) to the RFC3339
		// default-fraction case above, using TechZone's real wire format ---
		{
			name:           "space_separated_at_windowStart_eligible",
			provisionUntil: provisionUntilSpace,
			now:            time.Unix(provisionUntilEpoch-172800, 0).UTC(),
			durationDays:   4,
			windowFraction: 0.5,
			wantEligible:   true,
			wantOK:         true,
		},
		{
			name:           "space_separated_1s_before_windowStart_notEligible",
			provisionUntil: provisionUntilSpace,
			now:            time.Unix(provisionUntilEpoch-172800-1, 0).UTC(),
			durationDays:   4,
			windowFraction: 0.5,
			wantEligible:   false,
			wantOK:         true,
		},

		// --- degenerate fraction == 0.0: window opens exactly at provisionUntil
		// itself (validator permits this — spec §3d row 4; InExtensionWindow has
		// no lower-bound opinion) ---
		{
			name:           "zero_fraction_at_provisionUntil_eligible",
			provisionUntil: provisionUntilISO,
			now:            time.Unix(provisionUntilEpoch, 0).UTC(),
			durationDays:   4,
			windowFraction: 0.0,
			wantEligible:   true,
			wantOK:         true,
		},
		{
			name:           "zero_fraction_1s_before_provisionUntil_notEligible",
			provisionUntil: provisionUntilISO,
			now:            time.Unix(provisionUntilEpoch-1, 0).UTC(),
			durationDays:   4,
			windowFraction: 0.0,
			wantEligible:   false,
			wantOK:         true,
		},

		// --- no upper bound: far past provisionUntil is still eligible ---
		{
			name:           "no_upper_bound_100days_after_provisionUntil_eligible",
			provisionUntil: provisionUntilISO,
			now:            time.Unix(provisionUntilEpoch, 0).UTC().Add(100 * 24 * time.Hour),
			durationDays:   4,
			windowFraction: 0.5,
			wantEligible:   true,
			wantOK:         true,
		},
	}

	for _, tc := range rows {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotEligible, gotOK := techzone.InExtensionWindow(tc.provisionUntil, tc.now, tc.durationDays, tc.windowFraction)
			if gotOK != tc.wantOK {
				t.Errorf("InExtensionWindow(%q, now=%s, duration=%d, fraction=%v): ok = %v, want %v",
					tc.provisionUntil, tc.now.Format(time.RFC3339), tc.durationDays, tc.windowFraction, gotOK, tc.wantOK)
				return
			}
			if gotOK && gotEligible != tc.wantEligible {
				t.Errorf("InExtensionWindow(%q, now=%s, duration=%d, fraction=%v): eligible = %v, want %v",
					tc.provisionUntil, tc.now.Format(time.RFC3339), tc.durationDays, tc.windowFraction, gotEligible, tc.wantEligible)
			}
			// When ok==false, eligible MUST also be false — "cannot determine" is
			// never allowed to smuggle a stray true through the eligible return value.
			if !gotOK && gotEligible {
				t.Errorf("InExtensionWindow(%q, ...): ok=false but eligible=true — cannot-determine must always pair with eligible=false",
					tc.provisionUntil)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestNextExtensionDate
// ---------------------------------------------------------------------------

func TestNextExtensionDate(t *testing.T) {
	t.Parallel()

	type row struct {
		name           string
		provisionUntil string
		durationDays   int64
		wantOK         bool
		wantDate       string // only checked when wantOK == true
	}

	rows := []row{
		{
			name:           "durationDays_zero",
			provisionUntil: "2026-07-20T00:00:00Z",
			durationDays:   0,
			wantOK:         false,
		},
		{
			name:           "durationDays_negative",
			provisionUntil: "2026-07-20T00:00:00Z",
			durationDays:   -1,
			wantOK:         false,
		},
		{
			name:           "provisionUntil_empty",
			provisionUntil: "",
			durationDays:   4,
			wantOK:         false,
		},
		{
			name:           "provisionUntil_garbage",
			provisionUntil: "not-a-date",
			durationDays:   4,
			wantOK:         false,
		},
		{
			// Pinned (spec §2): independently re-verified via Go's own time package
			// in this environment: time.Date(2026,7,20,0,0,0,0,UTC).Add(4*24h)
			// .Format("2006-01-02T15:04:05.000Z") == "2026-07-24T00:00:00.000Z"
			// (epoch 1784851200).
			name:           "pinned_default_case",
			provisionUntil: "2026-07-20T00:00:00Z",
			durationDays:   4,
			wantOK:         true,
			wantDate:       "2026-07-24T00:00:00.000Z",
		},
		{
			// Space-separated regression: TechZone's real wire format must produce
			// the byte-identical output to the RFC3339 case above.
			name:           "space_separated_regression",
			provisionUntil: "2026-07-20 00:00:00",
			durationDays:   4,
			wantOK:         true,
			wantDate:       "2026-07-24T00:00:00.000Z",
		},
	}

	for _, tc := range rows {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, gotOK := techzone.NextExtensionDate(tc.provisionUntil, tc.durationDays)
			if gotOK != tc.wantOK {
				t.Errorf("NextExtensionDate(%q, %d): ok = %v, want %v", tc.provisionUntil, tc.durationDays, gotOK, tc.wantOK)
				return
			}
			if !tc.wantOK {
				if got != "" {
					t.Errorf("NextExtensionDate(%q, %d): ok=false but date = %q, want \"\"", tc.provisionUntil, tc.durationDays, got)
				}
				return
			}
			if got != tc.wantDate {
				t.Errorf("NextExtensionDate(%q, %d) = %q, want %q", tc.provisionUntil, tc.durationDays, got, tc.wantDate)
			}
		})
	}
}

// TestNextExtensionDate_InvariantAcrossDurations verifies, for a spread of
// durationDays values, that re-parsing NextExtensionDate's output via ToEpoch
// yields exactly ToEpoch(provisionUntil) + durationDays*86400 — i.e. the
// increment is always the full requested duration, no more, no less,
// regardless of the specific duration value. Mirrors the existing
// TestCreateEndDate_InvariantHoldsAcrossDurations convention in
// resource_reservation_end_date_unit_test.go (same repo, same style).
func TestNextExtensionDate_InvariantAcrossDurations(t *testing.T) {
	t.Parallel()

	const provisionUntil = "2026-07-20T00:00:00Z"
	baseEpoch, ok := techzone.ToEpoch(provisionUntil)
	if !ok {
		t.Fatalf("setup: ToEpoch(%q) unexpectedly failed", provisionUntil)
	}

	for _, days := range []int64{1, 4, 7, 30} {
		days := days
		t.Run(numDaysName(days), func(t *testing.T) {
			t.Parallel()
			gotDate, gotOK := techzone.NextExtensionDate(provisionUntil, days)
			if !gotOK {
				t.Fatalf("NextExtensionDate(%q, %d): ok = false, want true", provisionUntil, days)
			}
			gotEpoch, parsedOK := techzone.ToEpoch(gotDate)
			if !parsedOK {
				t.Fatalf("ToEpoch(%q) (re-parsing NextExtensionDate's own output) failed", gotDate)
			}
			wantEpoch := baseEpoch + days*86400
			if gotEpoch != wantEpoch {
				t.Errorf("NextExtensionDate(%q, %d) round-tripped to epoch %d, want %d (base %d + %d*86400)",
					provisionUntil, days, gotEpoch, wantEpoch, baseEpoch, days)
			}
		})
	}
}

// numDaysName renders a duration_days value as a subtest name fragment.
func numDaysName(days int64) string {
	switch days {
	case 1:
		return "duration_1d"
	case 4:
		return "duration_4d"
	case 7:
		return "duration_7d"
	case 30:
		return "duration_30d"
	default:
		return "duration_Nd"
	}
}
