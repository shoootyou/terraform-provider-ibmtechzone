package techzone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Sentinel errors — exported, errors.Is-comparable, wrappable with %w.
// ---------------------------------------------------------------------------

var (
	// ErrCollectionNotFound is returned when the API responds with HTTP 404.
	// Per TechZone API semantics, 404 covers both "does not exist" and "no read access".
	ErrCollectionNotFound = errors.New("collection not found")

	// ErrCollectionUnavailable is returned when the API responds with an HTTP 5xx status.
	ErrCollectionUnavailable = errors.New("collection unavailable")

	// ErrMalformedCollectionResponse is returned when the API responds with HTTP 200
	// but the body cannot be decoded into the expected collection structure.
	ErrMalformedCollectionResponse = errors.New("malformed collection response")

	// ErrEmptyPlatforms is returned when the API responds with HTTP 200 but the
	// platforms array is empty (len == 0).
	ErrEmptyPlatforms = errors.New("collection has no platforms")

	// ErrNoRegions is returned when platforms[0].regions is empty (len == 0).
	ErrNoRegions = errors.New("platform has no regions")

	// ErrUnsupportedInfrastructure is returned when platforms[0].infrastructure is
	// not "aws" (case-insensitive). Phase 1 aws-only gate.
	ErrUnsupportedInfrastructure = errors.New("unsupported infrastructure")
)

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

// PatternRef identifies a deployment pattern within a region.
type PatternRef struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Profile string `json:"profile"`
}

// Region describes a single deployable region within a platform.
type Region struct {
	Name          string     `json:"name"`
	Region        string     `json:"region"`
	Datacenter    string     `json:"datacenter"`
	Template      string     `json:"template"`
	RequestMethod string     `json:"requestMethod"`
	CloudAccount  string     `json:"cloudAccount"`
	Pattern       PatternRef `json:"pattern"`
}

// Platform describes a single infrastructure platform within a collection.
// Raw holds the verbatim JSON bytes of this platform element from the API
// response — populated via two-pass decode, not re-encoded from typed fields.
type Platform struct {
	Infrastructure string          `json:"infrastructure"`
	Regions        []Region        `json:"regions"`
	Raw            json.RawMessage `json:"-"`
}

// Collection is the top-level result of a GetCollection call.
type Collection struct {
	ID        string     `json:"id"`
	Platforms []Platform `json:"platforms"`
}

// ---------------------------------------------------------------------------
// GetCollection
// ---------------------------------------------------------------------------

// GetCollection fetches the TechZone collection identified by id from
// GET /api/collection/<id>. It uses the existing DoGet helper (bearer auth,
// redirect guard, 30 s timeout).
//
// Error taxonomy:
//   - HTTP 404                       → ErrCollectionNotFound
//   - HTTP 5xx                       → ErrCollectionUnavailable (wrapped)
//   - Transport failure              → connectivity error (not a sentinel)
//   - 200 + unparseable JSON         → ErrMalformedCollectionResponse (wrapped)
//   - 200 + empty platforms[]        → ErrEmptyPlatforms (wrapped)
//   - 200 + platforms[0].regions[]   → ErrNoRegions (wrapped)
//   - 200 + infrastructure != "aws"  → ErrUnsupportedInfrastructure (wrapped)
//   - 200 + valid aws collection     → (*Collection, nil)
//
// The bearer token is never included in any returned error string.
func (c *Client) GetCollection(ctx context.Context, id string) (*Collection, error) {
	status, body, err := c.DoGet(ctx, "/api/collection/"+id)
	if err != nil {
		// Transport/connectivity failure — return as-is (not a sentinel).
		return nil, err
	}

	switch {
	case status == 404:
		return nil, ErrCollectionNotFound

	case status >= 500 && status <= 599:
		return nil, fmt.Errorf("collection fetch failed (HTTP %d): %w", status, ErrCollectionUnavailable)
	}

	// HTTP 200 (or any non-404/5xx) — decode the body.

	// Pass 1: extract the outer envelope, capturing each platform element as raw bytes.
	var envelope struct {
		ID        string            `json:"id"`
		Platforms []json.RawMessage `json:"platforms"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decoding collection response: %w", ErrMalformedCollectionResponse)
	}

	// "platforms" key absent OR present but null decodes to a nil slice — treat as malformed
	// only if it's truly not a JSON array; json.Unmarshal into []json.RawMessage leaves it
	// nil when the field is absent. Distinguish: re-check for the key via a raw map probe.
	if envelope.Platforms == nil {
		// The "platforms" field was absent or null — not a usable collection.
		return nil, fmt.Errorf("collection response missing platforms field: %w", ErrMalformedCollectionResponse)
	}

	if len(envelope.Platforms) == 0 {
		return nil, fmt.Errorf("collection %q has no platforms: %w", envelope.ID, ErrEmptyPlatforms)
	}

	// Pass 2: unmarshal each raw element into a typed Platform and assign Raw.
	platforms := make([]Platform, 0, len(envelope.Platforms))
	for i, rawPlatform := range envelope.Platforms {
		var p Platform
		if err := json.Unmarshal(rawPlatform, &p); err != nil {
			return nil, fmt.Errorf("decoding platform[%d]: %w", i, ErrMalformedCollectionResponse)
		}
		p.Raw = rawPlatform // verbatim bytes — NOT re-encoded from typed fields
		platforms = append(platforms, p)
	}

	// Validate platforms[0] — the primary platform for Phase 1.
	primary := platforms[0]

	if !strings.EqualFold(primary.Infrastructure, "aws") {
		return nil, fmt.Errorf(
			"infrastructure %q is not supported (only aws is supported in Phase 1): %w",
			primary.Infrastructure,
			ErrUnsupportedInfrastructure,
		)
	}

	if len(primary.Regions) == 0 {
		return nil, fmt.Errorf("platform[0] has no regions: %w", ErrNoRegions)
	}

	return &Collection{
		ID:        envelope.ID,
		Platforms: platforms,
	}, nil
}
