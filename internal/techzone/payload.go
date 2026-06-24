// Package techzone — payload construction for the TechZone reservation API.
//
// BuildCreatePayload assembles the full POST /api/reservation/aws body from
// three inputs:
//   - platformRaw: verbatim bytes of platforms[0] returned by GET /api/collection/<id>
//   - dynamicOutputs: caller-supplied map of _NN_ keys → values (user-provided, no defaults)
//   - in: CreateInput holding the scalar fields that vary per reservation
//
// The platform bytes are embedded via json.RawMessage — NO re-encoding or field
// reordering (byte-identity requirement from RFC 022 §platform verbatim).
//
// Dynamic outputs are emitted in lexicographic key order (sort.Strings) in BOTH
// the dynamicOutputs[] array AND as flat top-level keys — determinism requirement
// per E10.
package techzone

import (
	"encoding/json"
	"sort"
)

// ---------------------------------------------------------------------------
// CreateInput — variable fields for each reservation
// ---------------------------------------------------------------------------

// CreateInput holds the fields that vary per reservation.
// All other payload fields are fixed constants in this package.
//
// Fields removed from the old signature (E3 refactor):
//   - User       — server derives identity from the bearer token (E8; never in payload)
//   - HCPOrg     — now caller-supplied via dynamicOutputs map (E2 decision B1)
//   - HCPProject — same as HCPOrg
//
// New fields added (derived from the selected collection region):
//   - Datacenter    — payload "datacenter" (empty string for cloud-account templates)
//   - Template      — payload "template" (from collection region)
//   - RequestMethod — payload "requestMethod" (from collection region)
//   - CloudAccount  — payload "cloudAccount" (from collection region)
type CreateInput struct {
	Name          string // payload "name"
	Purpose       string // payload "purpose"
	Region        string // payload "region"
	Datacenter    string // payload "datacenter" (empty for cloud-account templates)
	CollectionID  string // payload "collectionId"
	Template      string // payload "template" (from collection region)
	RequestMethod string // payload "requestMethod" (from collection region)
	CloudAccount  string // payload "cloudAccount" (from collection region)
	Start         string // ISO-8601 start timestamp
	End           string // ISO-8601 end timestamp
	// Optional fields: absent from payload when zero-value.
	Opportunity string // payload "opportunity" — omitted when ""
	IUI         string // payload "iui" — omitted when ""
}

// ---------------------------------------------------------------------------
// BuildCreatePayload — POST /api/reservation/aws body
// ---------------------------------------------------------------------------

// BuildCreatePayload assembles the full JSON body for POST /api/reservation/aws.
//
// Parameters:
//   - platformRaw: verbatim bytes of platforms[0] from GET /api/collection/<id>.
//     Embedded as json.RawMessage — NOT re-encoded through a struct, so key
//     order and whitespace are preserved byte-for-byte.
//   - dynamicOutputs: map of _NN_ output names → string values, supplied by
//     the caller. No provider defaults are injected. nil or empty → emits
//     "dynamicOutputs": [] and NO flat top-level _NN_ keys.
//   - in: scalar variable fields (see CreateInput).
//
// Fixed constants (from the disposition table, Phase 1):
//
//	reservationpurpose-0 = "Demo"
//	accountPool          = "any"
//	geo                  = "any"
//	customer             = ""
//	infrastructure       = "aws"
//	type                 = "reservation"
//	reservationtype-0    = "reservation"
//	terms                = true
//	customerData         = "false"
//	customerDataTypes    = []
//	opportunityProduct   = []
//	notes                = ""
//
// Absent keys:
//   - "user" is NEVER emitted (E8: server derives identity from bearer token).
//   - "opportunity" is omitted when CreateInput.Opportunity == "".
//   - "iui" is omitted when CreateInput.IUI == "".
func BuildCreatePayload(platformRaw json.RawMessage, dynamicOutputs map[string]string, in CreateInput) ([]byte, error) {
	// Collect and sort dynamic output keys for deterministic emission (E10).
	// Both the dynamicOutputs[] array and the flat top-level keys use this order.
	sortedKeys := make([]string, 0, len(dynamicOutputs))
	for k := range dynamicOutputs {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)

	// Build the dynamicOutputs array in sorted key order.
	dynArray := make([]map[string]string, 0, len(sortedKeys))
	for _, k := range sortedKeys {
		dynArray = append(dynArray, map[string]string{
			"name":  k,
			"value": dynamicOutputs[k],
		})
	}

	// Core payload — fixed and variable scalar fields.
	// "user" is intentionally absent (E8).
	payload := map[string]any{
		// Variable fields from CreateInput.
		"name":          in.Name,
		"purpose":       in.Purpose,
		"region":        in.Region,
		"datacenter":    in.Datacenter,
		"collectionId":  in.CollectionID,
		"template":      in.Template,
		"requestMethod": in.RequestMethod,
		"cloudAccount":  in.CloudAccount,
		"start":         in.Start,
		"end":           in.End,

		// platform: verbatim bytes from the collection API response.
		// json.RawMessage implements json.Marshaler and is embedded as-is —
		// no key reordering, no whitespace normalization.
		"platform": platformRaw,

		// dynamicOutputs: always present, even when empty (RFC §dual-emit).
		"dynamicOutputs": dynArray,

		// Fixed constants (Phase 1 disposition table).
		"reservationpurpose-0": "Demo",
		"accountPool":          "any",
		"geo":                  "any",
		"customer":             "",
		"infrastructure":       "aws",
		"type":                 "reservation",
		"reservationtype-0":    "reservation",
		"terms":                true,
		"customerData":         "false",
		"customerDataTypes":    []any{},
		"opportunityProduct":   []any{},
		"notes":                "",
	}

	// Flat top-level _NN_ keys — dual-emit in the same sorted order (E10).
	// Emitted only when dynamicOutputs is non-empty.
	for _, k := range sortedKeys {
		payload[k] = dynamicOutputs[k]
	}

	// Optional fields: emit only when non-empty.
	if in.Opportunity != "" {
		payload["opportunity"] = in.Opportunity
	}
	if in.IUI != "" {
		payload["iui"] = in.IUI
	}

	return json.Marshal(payload)
}
