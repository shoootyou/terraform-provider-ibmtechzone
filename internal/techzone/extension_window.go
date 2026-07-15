package techzone

import (
	"math"
	"time"
)

// DefaultExtensionWindowFraction is the fraction (0, 1] of the reservation's
// original duration, counted backward from provisionUntil, during which an
// extension attempt is eligible BY DEFAULT. 0.5 = eligible once inside the
// last 50% of reservation_duration_days.
//
// This is the single source of truth for the numeric default: it is wired
// directly into the ibmtechzone_reservation resource's Optional+Computed
// extension_window_fraction schema attribute via
// float64default.StaticFloat64(DefaultExtensionWindowFraction) in
// resource_reservation.go's Schema() (spec §3d) — do not duplicate this
// literal elsewhere. Callers of InExtensionWindow at runtime (Read()) pass
// the user-configured-or-defaulted value read from state, never this
// constant directly — this constant exists purely so the schema Default and
// any future consumer share one number.
const DefaultExtensionWindowFraction = 0.5

// InExtensionWindow reports whether now falls inside the extension window
// that precedes provisionUntil, and whether that determination could be made
// at all.
//
// Rules:
//   - durationDays <= 0, or provisionUntil unparseable via ToEpoch →
//     (false, false): "cannot determine" — deliberately distinct from "not
//     eligible" so the caller can log this case instead of silently treating
//     it as equivalent to the routine not-yet-eligible outcome (see
//     gotchas/ibm-techzone.md, fail-silent-to-KEEP finding).
//   - Otherwise: windowStart = ToEpoch(provisionUntil) - round(windowFraction*durationDays*86400)
//     (rounded, not truncated, to the nearest int64 second — see Ei/Sho
//     audit round 1 finding #4: truncating a float64 not exactly
//     representable in binary (e.g. 0.3, 0.7, 0.9) lands a fraction of a
//     second below the mathematically exact integer, which a strict-integer
//     boundary comparison then treats as "not yet eligible" one second early);
//     eligible = now.Unix() >= windowStart (inclusive lower
//     bound, no upper bound). Returns (eligible, true).
//   - Never calls time.Now() internally (now is always injected — mirrors
//     PastExpiry's existing contract, required for deterministic tests).
//   - windowFraction is caller-supplied (state.ExtensionWindowFraction at the
//     one real call site, spec §5) — this function does not read
//     DefaultExtensionWindowFraction itself and has no opinion on validity
//     bounds; validation of windowFraction happens at the schema layer
//     (spec §3d), not here.
func InExtensionWindow(provisionUntil string, now time.Time, durationDays int64, windowFraction float64) (eligible bool, ok bool) {
	if durationDays <= 0 {
		return false, false
	}
	epoch, parsed := ToEpoch(provisionUntil)
	if !parsed {
		return false, false
	}
	// Round to the nearest second rather than truncating (audit finding #4):
	// windowFraction*durationDays*24*60*60 is a float64 product that, for
	// fractions not exactly representable in binary (0.1, 0.3, 0.7, 0.9, ...),
	// lands a hair below the mathematically exact integer (e.g.
	// 0.3*1*86400 == 25919.999999999996, not 25920.0). int64() truncation
	// would silently drop that last second, moving windowStart one second
	// later than the contract requires — exactly the boundary the inclusive
	// "now >= windowStart" check depends on. math.Round recovers the exact
	// intended integer for every fraction, and is a no-op for the exactly
	// representable fractions already pinned in extension_window_test.go
	// (0.0, 0.25, 0.5).
	windowSeconds := int64(math.Round(windowFraction * float64(durationDays) * 24 * 60 * 60))
	return now.Unix() >= epoch-windowSeconds, true
}

// NextExtensionDate computes the new absolute provisionUntil to send as
// extensionDate: the current provisionUntil (parsed via ToEpoch) advanced by
// durationDays. Formatted with the exact layout Create() already uses for
// start/end ("2006-01-02T15:04:05.000Z", UTC) — no new wire format introduced.
//
// Returns ("", false) if provisionUntil is unparseable or durationDays <= 0
// (same fail-closed contract as InExtensionWindow — callers should treat this
// as "cannot compute", not "computed empty").
func NextExtensionDate(provisionUntil string, durationDays int64) (string, bool) {
	if durationDays <= 0 {
		return "", false
	}
	epoch, ok := ToEpoch(provisionUntil)
	if !ok {
		return "", false
	}
	next := time.Unix(epoch, 0).UTC().Add(time.Duration(durationDays) * 24 * time.Hour)
	return next.Format("2006-01-02T15:04:05.000Z"), true
}
