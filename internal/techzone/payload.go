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
	"fmt"
	"sort"
)

// reservedPayloadKeys is the set of top-level field names that BuildCreatePayload
// emits as structural fields. Any dynamic_outputs key that collides with a member
// of this set would silently overwrite a structural field — a category of injection
// risk documented in Ei audit finding F-02.
//
// The set covers every key emitted by BuildCreatePayload in its current form.
// Adding new structural fields to the payload map MUST be accompanied by a
// corresponding addition here.
var reservedPayloadKeys = map[string]struct{}{
	"platform":             {},
	"user":                 {},
	"type":                 {},
	"terms":                {},
	"collectionId":         {},
	"name":                 {},
	"purpose":              {},
	"region":               {},
	"datacenter":           {},
	"cloudAccount":         {},
	"dynamicOutputs":       {},
	"start":                {},
	"end":                  {},
	"template":             {},
	"requestMethod":        {},
	"infrastructure":       {},
	"iui":                  {},
	"opportunity":          {},
	"customer":             {},
	"customerData":         {},
	"customerDataTypes":    {},
	"reservationtype-0":    {},
	"reservationpurpose-0": {},
	"accountPool":          {},
	"geo":                  {},
	"notes":                {},
	"description":          {},
	"opportunityProduct":   {},
}

// ---------------------------------------------------------------------------
// CreateInput — variable fields for each reservation
// ---------------------------------------------------------------------------

// CreateInput holds the fields that vary per reservation.
// All other payload fields are fixed constants in this package.
//
// Fields removed from the old signature (E3 refactor):
//   - HCPOrg     — now caller-supplied via dynamicOutputs map (E2 decision B1)
//   - HCPProject — same as HCPOrg
//
// New fields added (derived from the selected collection region):
//   - Datacenter    — payload "datacenter" (empty string for cloud-account templates)
//   - Template      — payload "template" (from collection region)
//   - RequestMethod — payload "requestMethod" (from collection region)
//   - CloudAccount  — payload "cloudAccount" (from collection region)
//
// Live-validation fix (Plan 114 E8):
//   - User is REQUIRED — the live API returns HTTP 500 "Invalid user assignment" when
//     absent. E8 decision was a misread: server uses the submitted value for myId
//     assignment. Always emit "user" equal to CreateInput.User.
//   - Opportunity is []string — sending a JSON string causes HTTP 400. Emitted as a
//     JSON array when len > 0; omitted entirely when nil or empty.
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
	// Required — live API returns HTTP 500 when absent (Plan 114 live-validation fix).
	User string // payload "user" — always emitted
	// Optional fields: absent from payload when zero-value.
	Opportunity []string // payload "opportunity" — emitted as JSON array when len>0; omitted when nil/empty
	IUI         string   // payload "iui" — omitted when ""
}

// ---------------------------------------------------------------------------
// dynOutputEntry — typed element for the dynamicOutputs array
// ---------------------------------------------------------------------------

// dynOutputEntry is the element type for the dynamicOutputs[] array emitted in
// the POST /api/reservation/aws payload. Using a typed struct (rather than
// map[string]string) ensures consistent key ordering in the marshalled JSON.
type dynOutputEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
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
// Always-present keys:
//   - "user" MUST be emitted — live API returns HTTP 500 "Invalid user assignment"
//     when absent (Plan 114 live-validation fix; E8 decision was a misread).
//   - "description" is always "Terraform-managed reservation".
//
// Conditionally-present keys:
//   - "opportunity" is emitted as a JSON ARRAY when len(CreateInput.Opportunity) > 0;
//     OMITTED entirely when the slice is nil or empty. Sending a string instead of an
//     array causes the live API to return HTTP 400.
//   - "iui" is omitted when CreateInput.IUI == "".
func BuildCreatePayload(platformRaw json.RawMessage, dynamicOutputs map[string]string, in CreateInput) ([]byte, error) {
	// Guard 1 — json.Valid check at entry point (audit remediation item 2).
	// Explicit early failure with a clear message, before any other processing.
	// Prevents silent pass-through of invalid bytes into the outer json.Marshal.
	if !json.Valid(platformRaw) {
		return nil, fmt.Errorf("platformRaw is not valid JSON")
	}

	// Collect and sort dynamic output keys for deterministic emission (E10).
	// Both the dynamicOutputs[] array and the flat top-level keys use this order.
	sortedKeys := make([]string, 0, len(dynamicOutputs))
	for k := range dynamicOutputs {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)

	// Guard 2 — reserved-key collision check (audit remediation item 1, Ei F-02).
	// A dynamic_outputs key that matches a structural payload field would silently
	// overwrite it — e.g. key "platform" would replace the verbatim json.RawMessage
	// with a string, causing TechZone to return 500 (gotchas/techzone.md).
	for _, k := range sortedKeys {
		if _, reserved := reservedPayloadKeys[k]; reserved {
			return nil, fmt.Errorf("dynamic_outputs key %q collides with reserved payload field", k)
		}
	}

	// Build the dynamicOutputs array in sorted key order.
	dynArray := make([]dynOutputEntry, 0, len(sortedKeys))
	for _, k := range sortedKeys {
		dynArray = append(dynArray, dynOutputEntry{
			Name:  k,
			Value: dynamicOutputs[k],
		})
	}

	// Core payload — fixed and variable scalar fields.
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

		// "user": ALWAYS emitted — live API returns HTTP 500 when absent (Plan 114
		// live-validation fix). E8 decision was a misread; server uses this for myId.
		"user": in.User,

		// "description": constant — always present (live API expects it).
		"description": "Terraform-managed reservation",

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

	// "opportunity": emitted as a JSON ARRAY when len > 0; OMITTED when nil/empty.
	// Sending a string instead of an array causes HTTP 400 from the live API.
	if len(in.Opportunity) > 0 {
		payload["opportunity"] = in.Opportunity
	}
	// "iui": emitted as a string when non-empty; omitted otherwise.
	if in.IUI != "" {
		payload["iui"] = in.IUI
	}

	return json.Marshal(payload)
}
