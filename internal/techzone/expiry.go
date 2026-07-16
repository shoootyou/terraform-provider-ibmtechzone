package techzone

import (
	"strconv"
	"strings"
	"time"
)

// ToEpoch parses a provisionUntil string to UTC epoch seconds.
//
// Rules (RFC §3.2 / read.sh to_epoch):
//   - Trim surrounding double-quotes and ASCII whitespace.
//   - Empty string or the literal "null" → (0, false).
//   - All-digit string with ≥13 digits → epoch milliseconds → return (v/1000, true).
//   - All-digit string with <13 digits → epoch seconds → return (v, true).
//   - Otherwise: try RFC3339, then bare "2006-01-02T15:04:05" (treated as UTC),
//     then space-separated "2006-01-02 15:04:05" (no "T", no timezone marker,
//     treated as UTC — TechZone's real provisionUntil/provisionDate wire format).
//     Parse failure on all three → (0, false).
func ToEpoch(s string) (int64, bool) {
	// Trim surrounding double-quotes (TechZone sometimes wraps values in extra
	// quotes) and ASCII whitespace — mirrors read.sh `tr -d '"'` + whitespace trim.
	v := strings.TrimSpace(strings.Trim(s, `"`))

	// Empty or literal "null" → caller treats as KEEP.
	// "null" guard is load-bearing: jq -r '.provisionUntil // ""' renders a JSON
	// null as the 4-byte string "null" (read.sh:63).
	if v == "" || v == "null" {
		return 0, false
	}

	// All-digit branch.
	if isAllDigits(v) {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, false
		}
		// Cutover: ≥13 digits = epoch milliseconds; <13 digits = epoch seconds.
		// Pin is exactly >=13 (not >=12 or >13) — see F-7 test rows.
		if len(v) >= 13 {
			return n / 1000, true
		}
		return n, true
	}

	// ISO-8601 / RFC3339 branch: try with timezone marker first, then bare UTC.
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC().Unix(), true
	}
	if t, err := time.Parse("2006-01-02T15:04:05", v); err == nil {
		// Bare format carries no timezone; treat as UTC (mirrors read.sh `date -u -d`).
		return t.UTC().Unix(), true
	}
	// Layout: space-separator, no timezone — as returned by the real TechZone API.
	// Treat as UTC, consistent with the bare-T branch above.
	if t, err := time.Parse("2006-01-02 15:04:05", v); err == nil {
		return t.UTC().Unix(), true
	}

	return 0, false
}

// isAllDigits reports whether s consists entirely of ASCII decimal digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// PastExpiry reports whether provisionUntil is strictly in the past relative to now.
//
// Rules (RFC §3.2 / read.sh lines 75-78):
//   - now is injected (mandatory — the now==until boundary is untestable otherwise).
//   - If ToEpoch(provisionUntil) returns false → KEEP (return false).
//   - If now.Unix() > epoch → PRUNE (return true).
//   - If now.Unix() == epoch → KEEP (return false).  // strict >, NOT >=
//   - If now.Unix() < epoch → KEEP (return false).
//
// Never calls time.Now() internally.
func PastExpiry(provisionUntil string, now time.Time) bool {
	epoch, ok := ToEpoch(provisionUntil)
	if !ok {
		// Unparseable → cannot determine expiry → KEEP.
		return false
	}
	// Strict greater-than: now == until is KEEP, not PRUNE.
	return now.Unix() > epoch
}

// IsTerminalStatus reports whether status is a terminal prune signal.
//
// Rules (RFC §3.2 / read.sh lines 51-53):
//   - Exact, case-sensitive equality: true only for "Deleted" and "Expired".
//   - All other values (including "deleted", "DELETED", "Ready", "Provisioning",
//     "Queued", "") return false. Uses == not strings.EqualFold.
func IsTerminalStatus(status string) bool {
	return status == "Deleted" || status == "Expired"
}
