// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package techzone

import "time"

// ToEpoch parses a provisionUntil string to UTC epoch seconds.
//
// Rules (RFC §3.2 / read.sh to_epoch):
//   - Trim surrounding quotes and whitespace.
//   - Empty string or the literal "null" → (0, false).
//   - All-digit string with ≥13 digits → epoch milliseconds → return (v/1000, true).
//   - All-digit string with <13 digits → epoch seconds → return (v, true).
//   - Otherwise parse as UTC ISO-8601/RFC3339; parse failure → (0, false).
//
// TODO(kou): implement — E3 green phase.
func ToEpoch(s string) (int64, bool) {
	return 0, false
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
// TODO(kou): implement — E3 green phase.
func PastExpiry(provisionUntil string, now time.Time) bool {
	return false
}

// IsTerminalStatus reports whether status is a terminal prune signal.
//
// Rules (RFC §3.2 / read.sh lines 51-53):
//   - Exact, case-sensitive equality: true only for "Deleted" and "Expired".
//   - All other values (including "deleted", "DELETED", "Ready", "Provisioning",
//     "Queued", "") return false. Uses == not strings.EqualFold.
//
// TODO(kou): implement — E3 green phase.
func IsTerminalStatus(status string) bool {
	return false
}
