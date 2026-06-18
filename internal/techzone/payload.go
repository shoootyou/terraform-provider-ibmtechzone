// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package techzone — payload construction for the TechZone reservation API.
//
// All fields that are truly variable (name, purpose, user, region, collectionId,
// hcp_org, hcp_project, start, end) are parameters on BuildCreatePayload.
// Everything else — including the full platform object — is an unexported package
// variable so that any future upstream change to the "Hashicorp DDR" platform record
// requires updating exactly one var block, not hunting scattered constants.
package techzone

import "encoding/json"

// ---------------------------------------------------------------------------
// Fixed constants (internal — not user-configurable)
// ---------------------------------------------------------------------------

// platformRecord is the "Hashicorp DDR" platform object copied verbatim from
// create.sh.  It is isolated behind a single named var so it can be swapped if
// IBM modifies the upstream platform record (the embedded createdAt/updatedAt
// timestamps are a known fragility point — RFC §3.1).
var platformRecord = map[string]any{
	"id":               "69651c138d6e497dc77a8dbe",
	"createdAt":        int64(1768234003783),
	"updatedAt":        int64(1768235306788),
	"oid":              "69650af0758b9e41de66b6ae",
	"name":             "Hashicorp DDR",
	"description":      "Hashicorp DDR",
	"automationBucket": "public-solutions",
	"infrastructure":   "aws",
	"regions": []map[string]any{
		{
			"name":          "US East 2",
			"template":      "aws-account-hashicorp-ddr",
			"requestMethod": "aws-account-hashicorp-ddr",
			"cloudAccount":  "ITZ",
			"geo":           "",
			"region":        "us-east-2",
			"datacenter":    "",
			"status":        "Enabled",
			"pattern": map[string]any{
				"id":      "ccp-gitops/aws-account-hashicorp-ddr/itz",
				"name":    "aws-account-hashicorp-ddr",
				"profile": "default",
			},
			"variables":      []any{},
			"infrastructure": "aws",
			"profile":        "default",
		},
	},
	"status": "Enabled",
}

// Fixed scalar constants from create.sh — never user inputs.
var (
	fixedOpportunity    = []string{"006Ka00000NPHdITZSTG"}
	fixedIUI            = "2700013C3V"
	fixedTemplate       = "aws-account-hashicorp-ddr"
	fixedInfrastructure = "aws"
	fixedCloudAccount   = "ITZ"
	fixedAccountPool    = "any"
	fixedGeo            = "any"
	fixedDescription    = "Terraform-managed reservation"
	fixedAccountCleanup = "true"
)

// ---------------------------------------------------------------------------
// CreateInput — the variable fields for each reservation
// ---------------------------------------------------------------------------

// CreateInput holds the fields that vary per reservation.
// All other payload fields are fixed constants in this package.
type CreateInput struct {
	Name         string // payload "name" — reservation_name attribute
	Purpose      string // payload "purpose"
	User         string // payload "user" — user_email attribute
	Region       string // payload "region" and "datacenter"
	CollectionID string // payload "collectionId"
	HCPOrg       string // dynamicOutputs _04_hcp_org
	HCPProject   string // dynamicOutputs _05_hcp_project
	Start        string // ISO-8601 start timestamp (computed from now + 1 min)
	End          string // ISO-8601 end timestamp (computed from now + duration)
}

// ---------------------------------------------------------------------------
// BuildCreatePayload constructs the JSON body for POST /api/reservation/aws.
// ---------------------------------------------------------------------------

// BuildCreatePayload assembles the full XHR-replay payload from a CreateInput
// and returns the JSON-encoded bytes.  All fixed fields are taken from the
// unexported package vars above; only the fields in CreateInput vary.
//
// Transcribed faithfully from create.sh (the XHR replay against
// POST https://api.techzone.ibm.com/api/reservation/aws).
func BuildCreatePayload(in CreateInput) ([]byte, error) {
	payload := map[string]any{
		"name":               in.Name,
		"purpose":            in.Purpose,
		"customer":           "",
		"opportunity":        fixedOpportunity,
		"opportunityProduct": []any{},
		"description":        fixedDescription,
		"start":              in.Start,
		"end":                in.End,
		"notes":              "",
		"user":               in.User,
		"template":           fixedTemplate,
		"infrastructure":     fixedInfrastructure,
		"type":               "reservation",
		"requestMethod":      fixedTemplate,
		"region":             in.Region,
		"customerData":       "false",
		"customerDataTypes":  []any{},
		"iui":                fixedIUI,
		"collectionId":       in.CollectionID,
		"platform":           platformRecord,
		"terms":              true,
		"reservationtype-0":  "reservation",
		"dynamicOutputs": []map[string]string{
			{"name": "_03_account_cleanup", "value": fixedAccountCleanup},
			{"name": "_04_hcp_org", "value": in.HCPOrg},
			{"name": "_05_hcp_project", "value": in.HCPProject},
		},
		"_03_account_cleanup":  fixedAccountCleanup,
		"_04_hcp_org":          in.HCPOrg,
		"_05_hcp_project":      in.HCPProject,
		"reservationpurpose-0": "Demo",
		"accountPool":          fixedAccountPool,
		"geo":                  fixedGeo,
		"datacenter":           in.Region,
		"cloudAccount":         fixedCloudAccount,
	}

	return json.Marshal(payload)
}
