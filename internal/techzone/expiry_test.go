// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

// @spec-handoff
//
// @interface ToEpoch(s string) (int64, bool)
//
// @behavior
//   - Trim surrounding double-quotes and ASCII whitespace (mirrors read.sh
//     `tr -d '"'` followed by whitespace trim).
//   - After trimming: empty string OR the literal string "null"
//     → return (0, false).  Both cases map to KEEP in PastExpiry.
//     Rationale: jq -r '.provisionUntil // ""' renders a JSON null as the
//     4-byte string "null"; read.sh:63 has an explicit [[ "$v" == "null" ]] guard.
//   - All-digit string with len >= 13
//     → epoch milliseconds → return (v/1000, true).
//   - All-digit string with len < 13
//     → epoch seconds → return (v, true).
//   - The digit-branch cutover is exactly >= 13: a 12-digit input goes to the
//     epoch-seconds branch; a 13-digit input goes to epoch-milliseconds.
//     Never >= 12 or > 13.  Pinned by the 12-digit and 13-digit test rows (F-7).
//   - Otherwise: attempt time.Parse with RFC3339 (layout "2006-01-02T15:04:05Z07:00")
//     then time.Parse with bare ISO-8601 (layout "2006-01-02T15:04:05", treated as UTC).
//     On success → return (t.UTC().Unix(), true).
//     Both fail → return (0, false).
//
// @edge-cases
//   - "null"  (4 bytes)        → (0, false)          // C2 — jq -r null literal
//   - ""      (empty)          → (0, false)
//   - "  "    (whitespace)     → (0, false)           // trims to empty
//   - "not-a-date"             → (0, false)           // parse failure
//   - "1577836800"  (10 dig)   → (1577836800, true)   // epoch-sec
//   - "100000000000" (12 dig)  → (100000000000, true) // F-7: 12 dig = sec, NOT ms
//   - "1577836800000" (13 dig) → (1577836800, true)   // F-7: 13 dig = ms boundary
//   - "15778368000000" (14 dig)→ (15778368000, true)  // ms, >13
//   - ISO-8601 with Z          → (correct UTC epoch, true)
//   - ISO-8601 without Z       → (correct UTC epoch, true)  // treated as UTC
//
// @interface PastExpiry(provisionUntil string, now time.Time) bool
//
// @behavior
//   - Call ToEpoch(provisionUntil); if ok==false → return false (KEEP).
//   - If now.Unix() >  epoch → return true  (PRUNE).
//   - If now.Unix() == epoch → return false (KEEP).  // strict >, NOT >=; C1 boundary
//   - If now.Unix() <  epoch → return false (KEEP).
//   - now MUST be injected by the caller — never call time.Now() internally.
//     The now==until boundary is untestable without clock injection (RFC C1/F-3).
//
// @edge-cases
//   - provisionUntil = ""             → false (KEEP)
//   - provisionUntil = "null"         → false (KEEP)       // C2
//   - now == until (exact second)     → false (KEEP)       // C1 — strict >, not >=
//   - now == until - 1s (future)      → false (KEEP)
//   - now == until + 1s (past by 1s)  → true  (PRUNE)
//
// @interface IsTerminalStatus(status string) bool
//
// @behavior
//   - Return true for exactly "Deleted" and "Expired" (case-sensitive ==).
//   - Return false for everything else, including wrong-case variants, live
//     states, empty string, and any unknown/future status value.
//   - MUST use == not strings.EqualFold.  Case sensitivity is load-bearing:
//     a case-insensitive match would cause "Queued" reservations to prune on
//     every plan, triggering destructive false-positive recreates (F-4).
//
// @edge-cases
//   - "deleted" (lowercase) → false  // F-4 pin
//   - "DELETED" (uppercase) → false  // F-4 pin
//   - "Queued"              → false  // named live state; proves default→KEEP
//   - ""                   → false
//
// @see ./expiry.go  (stub signatures; Kou implements in E3)
// @see ./client.go  (HTTP transport; orthogonal to prune logic)
// @see RFC 015 §3.2 (.yui-soul/rfcs/approved/015-techzone-native-terraform-provider/README.md)
// @see read.sh (VCDLD-1678/repos/ddr-cloudaccounts-temp-aws-account/modules/techzone-reservation/scripts/read.sh)

package techzone_test

import (
	"testing"
	"time"

	"github.com/shoootyou-ext/terraform-provider-techzone/internal/techzone"
)

// ---------------------------------------------------------------------------
// TestToEpoch — parse provisionUntil string to UTC epoch seconds
// ---------------------------------------------------------------------------

func TestToEpoch(t *testing.T) {
	t.Parallel()

	type row struct {
		name      string
		input     string
		wantOK    bool
		wantEpoch int64 // only checked when wantOK == true
	}

	rows := []row{
		// --- unparseable inputs: (0, false) ---

		{
			name:   "empty_string",
			input:  "",
			wantOK: false,
		},
		{
			// C2: jq -r '.provisionUntil // ""' renders a JSON null as the 4-byte
			// string "null".  read.sh:63 has an explicit [[ "$v" == "null" ]] guard.
			// This must be KEEP, not parsed as a value.
			name:   "literal_null_C2",
			input:  "null",
			wantOK: false,
		},
		{
			name:   "whitespace_only", // trims to empty string
			input:  "  ",
			wantOK: false,
		},
		{
			name:   "garbage_string",
			input:  "not-a-date",
			wantOK: false,
		},

		// --- all-digit: epoch-seconds branch (len < 13) ---

		{
			// 10-digit epoch-second for 2020-01-01 00:00:00 UTC.
			name:      "epoch_sec_10digits",
			input:     "1577836800",
			wantOK:    true,
			wantEpoch: 1577836800,
		},
		{
			// F-7 pin: 12-digit input MUST go to the epoch-seconds branch (value as-is),
			// NOT to epoch-milliseconds (divide-by-1000).
			// 100000000000 seconds = year ~5138, far future.
			// A wrong implementation using >= 12 for the ms branch would divide by 1000
			// giving 100000000 s (~1973) — the PastExpiry test for this value would then
			// return PRUNE instead of KEEP, catching the mis-port.
			name:      "epoch_sec_12digits_F7",
			input:     "100000000000",
			wantOK:    true,
			wantEpoch: 100000000000,
		},

		// --- all-digit: epoch-milliseconds branch (len >= 13) ---

		{
			// F-7 pin: 13-digit is the exact threshold where ms-branch kicks in.
			// 1577836800000 ms / 1000 = 1577836800 s (2020-01-01 UTC).
			name:      "epoch_ms_13digits_boundary_F7",
			input:     "1577836800000",
			wantOK:    true,
			wantEpoch: 1577836800,
		},
		{
			// 14-digit millisecond value.
			name:      "epoch_ms_14digits",
			input:     "15778368000000",
			wantOK:    true,
			wantEpoch: 15778368000,
		},

		// --- ISO-8601 / RFC3339 ---

		{
			name:      "iso8601_with_Z",
			input:     "2020-01-01T00:00:00Z",
			wantOK:    true,
			wantEpoch: 1577836800,
		},
		{
			// Bare ISO-8601 with no timezone marker — treat as UTC (mirrors
			// read.sh `date -u -d "$v"` which forces UTC regardless of marker).
			name:      "iso8601_without_Z",
			input:     "2020-01-01T00:00:00",
			wantOK:    true,
			wantEpoch: 1577836800,
		},

		// --- quoted values: TechZone API sometimes wraps field values in extra quotes ---

		{
			// After stripping the surrounding double-quotes, the value becomes "null"
			// which must still return false.
			name:   "quoted_null",
			input:  `"null"`,
			wantOK: false,
		},
		{
			// After stripping the surrounding double-quotes, value is a valid RFC3339.
			name:      "quoted_iso8601",
			input:     `"2020-01-01T00:00:00Z"`,
			wantOK:    true,
			wantEpoch: 1577836800,
		},
	}

	for _, tc := range rows {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotEpoch, gotOK := techzone.ToEpoch(tc.input)
			if gotOK != tc.wantOK {
				t.Errorf("ToEpoch(%q): ok = %v, want %v", tc.input, gotOK, tc.wantOK)
				return
			}
			if tc.wantOK && gotEpoch != tc.wantEpoch {
				t.Errorf("ToEpoch(%q): epoch = %d, want %d", tc.input, gotEpoch, tc.wantEpoch)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestPastExpiry — PRUNE/KEEP decision with injected clock
// ---------------------------------------------------------------------------

func TestPastExpiry(t *testing.T) {
	t.Parallel()

	// fixedNow is the reference instant used in all relative-offset rows.
	// A fixed, well-known UTC time guarantees these tests are reproducible forever
	// regardless of when they run.  2024-06-15 12:00:00 UTC.
	fixedNow := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)

	// untilISO formats t as a "2006-01-02T15:04:05Z" string — the canonical
	// TechZone provisionUntil format.
	untilISO := func(t time.Time) string {
		return t.UTC().Format("2006-01-02T15:04:05Z")
	}

	type row struct {
		name  string
		until string
		now   time.Time
		want  bool // true = PRUNE, false = KEEP
	}

	rows := []row{
		// --- unparseable provisionUntil → KEEP (false) ---

		{
			name:  "unparseable_garbage",
			until: "not-a-date",
			now:   fixedNow,
			want:  false,
		},
		{
			// C2: jq -r renders JSON null as the literal string "null"; must KEEP.
			name:  "literal_null_C2",
			until: "null",
			now:   fixedNow,
			want:  false,
		},
		{
			name:  "empty_string",
			until: "",
			now:   fixedNow,
			want:  false,
		},

		// --- C1 boundary: strict > (NOT >=) ---
		// These three rows together prove the exact boundary.  A wrong >= implementation
		// would make the now==until row return PRUNE instead of KEEP.

		{
			// C1 critical: now == until exactly → KEEP.
			// This is the boundary that catches a >= mis-port.
			name:  "now_equals_until_KEEP_C1",
			until: untilISO(fixedNow),
			now:   fixedNow,
			want:  false,
		},
		{
			// until is 1 second in the future → KEEP.
			name:  "now_before_until_1s_KEEP",
			until: untilISO(fixedNow.Add(1 * time.Second)),
			now:   fixedNow,
			want:  false,
		},
		{
			// until was 1 second ago → PRUNE.
			name:  "now_after_until_1s_PRUNE",
			until: untilISO(fixedNow.Add(-1 * time.Second)),
			now:   fixedNow,
			want:  true,
		},

		// --- clearly-past ISO timestamp → PRUNE ---

		{
			// 2020-01-01 is well before fixedNow (2024-06-15).
			name:  "clearly_past_iso_PRUNE",
			until: "2020-01-01T00:00:00Z",
			now:   fixedNow,
			want:  true,
		},

		// --- clearly-future ISO timestamp → KEEP ---

		{
			// 2099-01-01 is well after fixedNow.
			name:  "clearly_future_iso_KEEP",
			until: "2099-01-01T00:00:00Z",
			now:   fixedNow,
			want:  false,
		},

		// --- epoch-second string (10-digit, clearly past) → PRUNE ---

		{
			// 1577836800 = 2020-01-01 00:00:00 UTC, well before fixedNow.
			name:  "epoch_sec_past_PRUNE",
			until: "1577836800",
			now:   fixedNow,
			want:  true,
		},

		// --- epoch-millisecond string (13-digit, clearly past) → PRUNE ---

		{
			// F-7: 13-digit → ms/1000 = 2020-01-01 UTC < fixedNow → PRUNE.
			name:  "epoch_ms_13digit_past_PRUNE_F7",
			until: "1577836800000",
			now:   fixedNow,
			want:  true,
		},

		// --- 12-digit epoch-second (F-7: must NOT be treated as ms) → KEEP ---

		{
			// F-7 pin: 100000000000 seconds ≈ year 5138 — far future → KEEP.
			// If the impl mistakenly uses >= 12 for the ms branch, it divides by 1000
			// giving 100000000 s (≈1973) which is past fixedNow → returns PRUNE.
			// The test would then FAIL, catching the mis-port.
			name:  "epoch_12digit_sec_future_KEEP_F7",
			until: "100000000000",
			now:   fixedNow,
			want:  false,
		},
	}

	for _, tc := range rows {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := techzone.PastExpiry(tc.until, tc.now)
			if got != tc.want {
				verdict := map[bool]string{true: "PRUNE", false: "KEEP"}
				t.Errorf("PastExpiry(%q, now=%d [%s]): got %s, want %s",
					tc.until,
					tc.now.Unix(), tc.now.UTC().Format(time.RFC3339),
					verdict[got], verdict[tc.want],
				)
			}
		})
	}
}

// TestPastExpiry_NowEqualsUntilEpoch_Strict pins the C1 boundary using a raw
// epoch-integer provisionUntil, independent of any ISO formatting round-trip.
// This is a belt-and-suspenders check: the table row above uses an ISO string
// that goes through the full ToEpoch parse path; this row uses the epoch-seconds
// decimal directly, so both paths are exercised for the exact-second boundary.
func TestPastExpiry_NowEqualsUntilEpoch_Strict(t *testing.T) {
	t.Parallel()
	// 1577836800 = 2020-01-01 00:00:00 UTC.
	const anchorEpoch = int64(1577836800)
	now := time.Unix(anchorEpoch, 0).UTC()

	// ToEpoch("1577836800") must return (anchorEpoch, true) and
	// PastExpiry must return false (KEEP) when now.Unix() == anchorEpoch.
	got := techzone.PastExpiry("1577836800", now)
	if got {
		t.Errorf(
			"PastExpiry(%q, now=%d): got PRUNE, want KEEP — "+
				"now==until must be KEEP (strict >, not >=); C1 boundary",
			"1577836800", anchorEpoch,
		)
	}
}

// ---------------------------------------------------------------------------
// TestIsTerminalStatus — exact case-sensitive allowlist
// ---------------------------------------------------------------------------

func TestIsTerminalStatus(t *testing.T) {
	t.Parallel()

	type row struct {
		name   string
		status string
		want   bool
	}

	rows := []row{
		// --- allowlist members: true (PRUNE) ---

		{name: "Deleted", status: "Deleted", want: true},
		{name: "Expired", status: "Expired", want: true},

		// --- live / non-terminal states: false (KEEP) ---

		{name: "Ready", status: "Ready", want: false},
		{name: "Provisioning", status: "Provisioning", want: false},
		// F-4: "Queued" is a real live state observed in the wild.
		// A case-insensitive match would treat "Queued" as non-terminal correctly,
		// but this named row proves the switch default-to-KEEP path is exercised.
		{name: "Queued_F4_named_live_state", status: "Queued", want: false},
		// "Failed" is NOT in the allowlist — hard error in the resource's Read; KEEP here.
		{name: "Failed", status: "Failed", want: false},

		// --- case-sensitivity enforcement (F-4: == not strings.EqualFold) ---

		// These rows are the critical proof that the implementation does NOT use
		// EqualFold.  If it did, "deleted" would return true (PRUNE), causing
		// live reservations whose status is returned in unexpected case to flap.
		{name: "lowercase_deleted_F4", status: "deleted", want: false},
		{name: "uppercase_DELETED_F4", status: "DELETED", want: false},
		{name: "lowercase_expired_F4", status: "expired", want: false},
		{name: "uppercase_EXPIRED_F4", status: "EXPIRED", want: false},
		{name: "mixed_case_dEleted_F4", status: "dEleted", want: false},

		// --- edge values ---

		{name: "empty_string", status: "", want: false},
		{name: "unknown_Foo", status: "Foo", want: false},
		{name: "hypothetical_Archiving", status: "Archiving", want: false},
	}

	for _, tc := range rows {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := techzone.IsTerminalStatus(tc.status)
			if got != tc.want {
				t.Errorf("IsTerminalStatus(%q): got %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}
