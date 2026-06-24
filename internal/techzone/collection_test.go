/**
 * @spec-handoff
 *
 * @interface (*Client).GetCollection(ctx context.Context, id string) (*Collection, error)
 *
 * HTTP target: GET /api/collection/<id>
 * Auth:        Authorization: Bearer <api_key>   (via existing DoGet — NEVER in URL)
 *
 * @structs
 *
 *   type PatternRef struct {
 *       ID      string `json:"id"`
 *       Name    string `json:"name"`
 *       Profile string `json:"profile"`
 *   }
 *
 *   type Region struct {
 *       Name          string `json:"name"`
 *       Region        string `json:"region"`
 *       Datacenter    string `json:"datacenter"`
 *       Template      string `json:"template"`
 *       RequestMethod string `json:"requestMethod"`
 *       CloudAccount  string `json:"cloudAccount"`
 *       Pattern       PatternRef `json:"pattern"`
 *   }
 *
 *   type Platform struct {
 *       Infrastructure string          `json:"infrastructure"`
 *       Regions        []Region        `json:"regions"`
 *       Raw            json.RawMessage `json:"-"` // verbatim bytes of this platform element
 *   }
 *
 *   type Collection struct {
 *       ID        string     `json:"id"`
 *       Platforms []Platform `json:"platforms"`
 *   }
 *
 * Platform.Raw is populated via two-pass decode:
 *   1. Unmarshal outer body into struct{ Platforms []json.RawMessage }
 *   2. For each element: unmarshal into Platform, assign Platform.Raw = rawBytes[i]
 * This guarantees Platform.Raw is the VERBATIM bytes of platforms[i] from the API response.
 *
 * @behavior
 *   - Calls DoGet(ctx, "/api/collection/"+id)
 *   - On HTTP 404: returns nil, ErrCollectionNotFound (sentinel)
 *   - On HTTP 5xx: returns nil, fmt.Errorf("collection fetch failed: %w", ErrCollectionUnavailable)
 *   - On transport failure (dial/timeout): returns nil, wrapped transport error
 *     (NOT ErrCollectionNotFound, NOT ErrCollectionUnavailable)
 *   - On HTTP 200 with unparseable / structurally wrong JSON: returns nil,
 *     fmt.Errorf("...: %w", ErrMalformedCollectionResponse)
 *   - On HTTP 200 with platforms[] empty (len == 0): returns nil,
 *     fmt.Errorf("...: %w", ErrEmptyPlatforms)
 *   - On HTTP 200 with platforms[0].regions[] empty (len == 0): returns nil,
 *     fmt.Errorf("...: %w", ErrNoRegions)
 *   - On HTTP 200 with platforms[0].infrastructure != "aws" (case-insensitive):
 *     returns nil, fmt.Errorf("...: %w", ErrUnsupportedInfrastructure)
 *   - On HTTP 200 with valid aws platform+regions: returns (*Collection, nil)
 *     where Collection.Platforms[0].Raw holds the VERBATIM bytes of platforms[0]
 *     and typed Region fields decode correctly.
 *
 * @edge-cases
 *   - infrastructure comparison is case-insensitive (strings.EqualFold)
 *   - Platform.Raw must be byte-identical to the raw JSON element, including
 *     key order and whitespace — NOT a re-encoding from the typed struct
 *   - Token value MUST NOT appear in any returned error string (token-safety contract)
 *   - Transport errors are a distinct class: errors.Is(err, ErrCollectionNotFound) == false
 *     and errors.Is(err, ErrCollectionUnavailable) == false
 *
 * @error-taxonomy (all are exported sentinel vars, errors.Is-comparable)
 *   ErrCollectionNotFound          — HTTP 404 (not found OR no read access)
 *   ErrCollectionUnavailable       — HTTP 5xx (upstream error)
 *   ErrMalformedCollectionResponse — 200 with unparseable / structurally invalid JSON
 *   ErrEmptyPlatforms              — 200 but platforms[] is empty
 *   ErrNoRegions                   — 200 but selected platform has zero regions
 *   ErrUnsupportedInfrastructure   — 200 but infrastructure != "aws" (Phase 1 aws-only gate)
 *
 * @see ./collection.go   (Kou implements here — file does not exist yet; tests are RED)
 * @see ./client.go       (DoGet, NewClient, bearer auth, redirect guard)
 * @see ../../.yui-soul/plans/wip/114-techzone-template-agnostic/e2-spec-platform-raw.md
 * @see ../../.yui-soul/rfcs/approved/022-techzone-template-agnostic/README.md §GetCollection
 */

package techzone_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shoootyou-ext/terraform-provider-ibmtechzone/internal/techzone"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// collectionFixtureAWS is a minimal but realistic collection JSON fixture
// representing a single-platform, single-region DDR-shaped AWS collection.
// The platform element is captured verbatim so tests can assert Platform.Raw.
//
// NOTE: keep the exact byte layout — Platform.Raw must equal the platforms[0]
// element (from the opening "{" of that element to its closing "}") verbatim.
const collectionFixtureAWS = `{
  "id": "69650af0758b9e41de66b6ae",
  "platforms": [
    {
      "id": "69651c138d6e497dc77a8dbe",
      "oid": "62ccb18c2d38520017eec9fb",
      "name": "Reservation Name",
      "infrastructure": "aws",
      "regions": [
        {
          "name": "US East 2",
          "region": "us-east-2",
          "datacenter": "",
          "template": "aws-account-hashicorp-ddr",
          "requestMethod": "aws-account-hashicorp-ddr",
          "cloudAccount": "ITZ",
          "pattern": {
            "id": "ccp-gitops/aws-account-hashicorp-ddr/itz",
            "name": "aws-account-hashicorp-ddr",
            "profile": "itz"
          }
        }
      ]
    }
  ]
}`

// platformRawFixture is the bytes of platforms[0] as they appear in
// collectionFixtureAWS — exactly what Platform.Raw must equal.
// (json.RawMessage comparisons are byte-level; whitespace is preserved.)
const platformRawFixture = `{
      "id": "69651c138d6e497dc77a8dbe",
      "oid": "62ccb18c2d38520017eec9fb",
      "name": "Reservation Name",
      "infrastructure": "aws",
      "regions": [
        {
          "name": "US East 2",
          "region": "us-east-2",
          "datacenter": "",
          "template": "aws-account-hashicorp-ddr",
          "requestMethod": "aws-account-hashicorp-ddr",
          "cloudAccount": "ITZ",
          "pattern": {
            "id": "ccp-gitops/aws-account-hashicorp-ddr/itz",
            "name": "aws-account-hashicorp-ddr",
            "profile": "itz"
          }
        }
      ]
    }`

// collectionFixtureVMware has infrastructure "vmware" to trigger
// ErrUnsupportedInfrastructure (Phase 1 aws-only gate).
const collectionFixtureVMware = `{
  "id": "aabbccddeeff001122334455",
  "platforms": [
    {
      "id": "platform-vmware-001",
      "name": "VMware Lab",
      "infrastructure": "vmware",
      "regions": [
        {
          "name": "DC1",
          "region": "dc1",
          "datacenter": "dc-west",
          "template": "vmware-template",
          "requestMethod": "vmware-template",
          "cloudAccount": "ACME",
          "pattern": {
            "id": "ccp-gitops/vmware/acme",
            "name": "vmware-lab",
            "profile": "acme"
          }
        }
      ]
    }
  ]
}`

// collectionFixtureEmptyPlatforms has a platforms array with zero elements.
const collectionFixtureEmptyPlatforms = `{
  "id": "000000000000000000000001",
  "platforms": []
}`

// collectionFixtureNoRegions has a platform with zero regions.
const collectionFixtureNoRegions = `{
  "id": "000000000000000000000002",
  "platforms": [
    {
      "id": "platform-no-regions",
      "name": "Empty Platform",
      "infrastructure": "aws",
      "regions": []
    }
  ]
}`

// collectionFixtureMalformed is valid JSON but does not have a usable
// "platforms" field — triggers ErrMalformedCollectionResponse.
const collectionFixtureMalformed = `{"garbage": true, "notPlatforms": 42}`

// ---------------------------------------------------------------------------
// Helper: spin up a one-shot httptest.Server returning the given status+body.
// ---------------------------------------------------------------------------

func newCollectionServer(t *testing.T, statusCode int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newCollectionClient builds a *techzone.Client pointed at srv.URL.
func newCollectionClient(t *testing.T, srv *httptest.Server) *techzone.Client {
	t.Helper()
	c, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// ---------------------------------------------------------------------------
// Test cases — 8 taxonomy cases required by E4 Task 1
// ---------------------------------------------------------------------------

// Case 1 — Happy path: 200 + valid AWS collection JSON.
//
// Asserts:
//   - No error returned.
//   - Collection.ID matches fixture.
//   - Collection.Platforms is non-empty and Platforms[0].Regions is non-empty.
//   - Platforms[0].Infrastructure == "aws".
//   - Platforms[0].Regions[0] typed fields decode correctly.
//   - Platforms[0].Raw holds the VERBATIM bytes of platforms[0] (two-pass decode).
func TestGetCollection_HappyPath_ReturnsCollectionWithRawAndTypedFields(t *testing.T) {
	t.Parallel()

	srv := newCollectionServer(t, http.StatusOK, collectionFixtureAWS)
	c := newCollectionClient(t, srv)

	col, err := c.GetCollection(context.Background(), "69650af0758b9e41de66b6ae")
	if err != nil {
		t.Fatalf("GetCollection returned unexpected error: %v", err)
	}
	if col == nil {
		t.Fatal("GetCollection returned nil Collection on success")
	}

	// Collection-level fields.
	if col.ID != "69650af0758b9e41de66b6ae" {
		t.Errorf("Collection.ID = %q, want %q", col.ID, "69650af0758b9e41de66b6ae")
	}
	if len(col.Platforms) == 0 {
		t.Fatal("Collection.Platforms is empty; want at least one platform")
	}

	p := col.Platforms[0]

	// Typed platform fields.
	if p.Infrastructure != "aws" {
		t.Errorf("Platform.Infrastructure = %q, want %q", p.Infrastructure, "aws")
	}
	if len(p.Regions) == 0 {
		t.Fatal("Platform.Regions is empty; want at least one region")
	}

	r := p.Regions[0]
	if r.Region != "us-east-2" {
		t.Errorf("Region.Region = %q, want %q", r.Region, "us-east-2")
	}
	if r.RequestMethod != "aws-account-hashicorp-ddr" {
		t.Errorf("Region.RequestMethod = %q, want %q", r.RequestMethod, "aws-account-hashicorp-ddr")
	}
	if r.CloudAccount != "ITZ" {
		t.Errorf("Region.CloudAccount = %q, want %q", r.CloudAccount, "ITZ")
	}
	if r.Pattern.ID != "ccp-gitops/aws-account-hashicorp-ddr/itz" {
		t.Errorf("Region.Pattern.ID = %q, want %q", r.Pattern.ID, "ccp-gitops/aws-account-hashicorp-ddr/itz")
	}

	// Platform.Raw must be non-nil and hold the VERBATIM bytes of platforms[0].
	// Two-pass decode contract: the raw bytes come from []json.RawMessage intermediate,
	// NOT from re-encoding the typed struct — so byte identity is preserved.
	if len(p.Raw) == 0 {
		t.Fatal("Platform.Raw is empty; want verbatim bytes of platforms[0]")
	}
	if string(p.Raw) != platformRawFixture {
		t.Errorf("Platform.Raw byte mismatch:\ngot:  %s\nwant: %s", string(p.Raw), platformRawFixture)
	}
}

// Case 2 — 404: server returns 404 → ErrCollectionNotFound.
//
// Per RFC §GetCollection and E7: the TechZone API returns 404 both for
// "collection does not exist" and "token lacks read access".
// A single sentinel covers both cases.
func TestGetCollection_404_ReturnsErrCollectionNotFound(t *testing.T) {
	t.Parallel()

	srv := newCollectionServer(t, http.StatusNotFound, `{"error":"not found"}`)
	c := newCollectionClient(t, srv)

	_, err := c.GetCollection(context.Background(), "nonexistent-id")
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	if !errors.Is(err, techzone.ErrCollectionNotFound) {
		t.Errorf("errors.Is(err, ErrCollectionNotFound) == false; got: %v", err)
	}
	// Token-safety: error string must not contain the sentinel token.
	assertNoTokenLeak(t, err.Error())
}

// Case 3 — 5xx: server returns 500 → error wraps ErrCollectionUnavailable.
func TestGetCollection_5xx_ReturnsErrCollectionUnavailable(t *testing.T) {
	t.Parallel()

	srv := newCollectionServer(t, http.StatusInternalServerError, `{"error":"internal server error"}`)
	c := newCollectionClient(t, srv)

	_, err := c.GetCollection(context.Background(), "any-id")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
	if !errors.Is(err, techzone.ErrCollectionUnavailable) {
		t.Errorf("errors.Is(err, ErrCollectionUnavailable) == false; got: %v", err)
	}
	// Must NOT be mistakenly identified as a not-found error.
	if errors.Is(err, techzone.ErrCollectionNotFound) {
		t.Error("5xx error must not satisfy errors.Is(ErrCollectionNotFound)")
	}
	assertNoTokenLeak(t, err.Error())
}

// Case 4 — Transport failure: closed server → connectivity-class error.
//
// Asserts the error is non-nil and is NOT a sentinel HTTP-status error —
// it must be a transport/connectivity class error, distinct from 404/5xx sentinels.
func TestGetCollection_TransportFailure_ReturnsConnectivityError(t *testing.T) {
	t.Parallel()

	// Start a server then close it immediately — port becomes unreachable.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	c := newCollectionClient(t, srv)
	srv.Close() // close AFTER building the client so the URL is valid

	_, err := c.GetCollection(context.Background(), "any-id")
	if err == nil {
		t.Fatal("expected transport error for closed server, got nil")
	}
	// Must NOT be a sentinel HTTP-status error.
	if errors.Is(err, techzone.ErrCollectionNotFound) {
		t.Error("transport error must not satisfy errors.Is(ErrCollectionNotFound)")
	}
	if errors.Is(err, techzone.ErrCollectionUnavailable) {
		t.Error("transport error must not satisfy errors.Is(ErrCollectionUnavailable)")
	}
	// Token-safety.
	assertNoTokenLeak(t, err.Error())
}

// Case 5 — Malformed JSON: 200 with structurally wrong body →
// error wraps ErrMalformedCollectionResponse.
func TestGetCollection_MalformedJSON_ReturnsErrMalformedCollectionResponse(t *testing.T) {
	t.Parallel()

	srv := newCollectionServer(t, http.StatusOK, collectionFixtureMalformed)
	c := newCollectionClient(t, srv)

	_, err := c.GetCollection(context.Background(), "any-id")
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	if !errors.Is(err, techzone.ErrMalformedCollectionResponse) {
		t.Errorf("errors.Is(err, ErrMalformedCollectionResponse) == false; got: %v", err)
	}
	assertNoTokenLeak(t, err.Error())
}

// Case 6 — Empty platforms array: 200 + {"platforms":[]} →
// error wraps ErrEmptyPlatforms.
func TestGetCollection_EmptyPlatforms_ReturnsErrEmptyPlatforms(t *testing.T) {
	t.Parallel()

	srv := newCollectionServer(t, http.StatusOK, collectionFixtureEmptyPlatforms)
	c := newCollectionClient(t, srv)

	_, err := c.GetCollection(context.Background(), "any-id")
	if err == nil {
		t.Fatal("expected error for empty platforms array, got nil")
	}
	if !errors.Is(err, techzone.ErrEmptyPlatforms) {
		t.Errorf("errors.Is(err, ErrEmptyPlatforms) == false; got: %v", err)
	}
	assertNoTokenLeak(t, err.Error())
}

// Case 7 — Platform index 0 present but regions[] is empty →
// error wraps ErrNoRegions.
//
// This exercises the bounds-check after the infrastructure check:
// platforms[0] exists but has zero regions → cannot derive a region block.
func TestGetCollection_NoRegions_ReturnsErrNoRegions(t *testing.T) {
	t.Parallel()

	srv := newCollectionServer(t, http.StatusOK, collectionFixtureNoRegions)
	c := newCollectionClient(t, srv)

	_, err := c.GetCollection(context.Background(), "any-id")
	if err == nil {
		t.Fatal("expected error for platform with no regions, got nil")
	}
	if !errors.Is(err, techzone.ErrNoRegions) {
		t.Errorf("errors.Is(err, ErrNoRegions) == false; got: %v", err)
	}
	assertNoTokenLeak(t, err.Error())
}

// Case 8 — Unsupported infrastructure: platforms[0].infrastructure == "vmware"
// (non-aws) → error wraps ErrUnsupportedInfrastructure.
//
// Phase 1 aws-only gate: per RFC §GetCollection and E4/Task2, infrastructure
// is checked case-insensitively (strings.EqualFold). "vmware" != "aws".
func TestGetCollection_NonAWSInfrastructure_ReturnsErrUnsupportedInfrastructure(t *testing.T) {
	t.Parallel()

	srv := newCollectionServer(t, http.StatusOK, collectionFixtureVMware)
	c := newCollectionClient(t, srv)

	_, err := c.GetCollection(context.Background(), "aabbccddeeff001122334455")
	if err == nil {
		t.Fatal("expected error for vmware infrastructure, got nil")
	}
	if !errors.Is(err, techzone.ErrUnsupportedInfrastructure) {
		t.Errorf("errors.Is(err, ErrUnsupportedInfrastructure) == false; got: %v", err)
	}
	// Must NOT be mistakenly identified as other sentinels.
	if errors.Is(err, techzone.ErrCollectionNotFound) {
		t.Error("vmware error must not satisfy errors.Is(ErrCollectionNotFound)")
	}
	if errors.Is(err, techzone.ErrEmptyPlatforms) {
		t.Error("vmware error must not satisfy errors.Is(ErrEmptyPlatforms)")
	}
	assertNoTokenLeak(t, err.Error())
}

// ---------------------------------------------------------------------------
// Sentinel identity check — exported sentinels are package-level vars, not
// inline literals, so errors.Is comparisons are stable across wrapping chains.
// ---------------------------------------------------------------------------

// TestGetCollection_SentinelIdentity verifies that each exported sentinel is a
// distinct, non-nil error value — guards against package-level var aliasing.
func TestGetCollection_SentinelIdentity_AllDistinct(t *testing.T) {
	t.Parallel()

	sentinels := []struct {
		name string
		err  error
	}{
		{"ErrCollectionNotFound", techzone.ErrCollectionNotFound},
		{"ErrCollectionUnavailable", techzone.ErrCollectionUnavailable},
		{"ErrMalformedCollectionResponse", techzone.ErrMalformedCollectionResponse},
		{"ErrEmptyPlatforms", techzone.ErrEmptyPlatforms},
		{"ErrNoRegions", techzone.ErrNoRegions},
		{"ErrUnsupportedInfrastructure", techzone.ErrUnsupportedInfrastructure},
	}

	// All must be non-nil.
	for _, s := range sentinels {
		if s.err == nil {
			t.Errorf("sentinel %s is nil; must be a non-nil exported var", s.name)
		}
	}

	// All pairs must be distinct (no aliasing).
	for i := 0; i < len(sentinels); i++ {
		for j := i + 1; j < len(sentinels); j++ {
			if errors.Is(sentinels[i].err, sentinels[j].err) {
				t.Errorf("sentinel aliasing: errors.Is(%s, %s) is true; they must be distinct",
					sentinels[i].name, sentinels[j].name)
			}
		}
	}
}
