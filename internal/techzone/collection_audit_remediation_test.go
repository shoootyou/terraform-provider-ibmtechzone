/**
 * @spec-handoff — Audit Remediation (Round 1) — collection.go contracts
 *
 * @interface (*Client).GetCollection(ctx context.Context, id string) (*Collection, error)
 *
 * @behavior (additions to existing spec — audit remediation)
 *
 *   PATH-ESCAPE CONTRACT:
 *   - The collection id MUST be url.PathEscape'd before concatenation into the
 *     request path. The implementation must send GET /api/collection/<escaped-id>,
 *     NOT GET /api/collection/<raw-id> when id contains path-significant chars.
 *   - A collection id containing "/" (e.g. "a/b", "../foo") must be escaped so
 *     that the actual HTTP path does not include an unescaped slash from the id.
 *   - Example: id = "a/b" → path must be /api/collection/a%2Fb (NOT /api/collection/a/b).
 *   - Example: id = "../foo" → path must be /api/collection/..%2Ffoo.
 *   - A normal hex-ID (e.g. "69650af0758b9e41de66b6ae") must pass through unchanged
 *     (url.PathEscape of a hex string is identity).
 *
 *   401/403 MAPPING CONTRACT:
 *   - HTTP 401 (Unauthorized) MUST return ErrCollectionNotFound.
 *     Rationale (Ei F-01 / Sho-core finding #3): 401/403 on a collection endpoint
 *     means "access denied" — functionally the same as "not found" for the caller;
 *     exposing it as ErrMalformedCollectionResponse (the current behavior) is
 *     misleading and makes the error actionable message incorrect.
 *   - HTTP 403 (Forbidden) MUST return ErrCollectionNotFound.
 *   - errors.Is(err, ErrCollectionNotFound) must be true for both 401 and 403.
 *   - errors.Is(err, ErrMalformedCollectionResponse) must be false for both 401 and 403.
 *
 *   INVALID-JSON BODY CONTRACT:
 *   - HTTP 200 with a body that is syntactically invalid JSON (not merely missing
 *     the "platforms" key) MUST return ErrMalformedCollectionResponse.
 *   - Both paths through the malformed-body branch (json.Unmarshal error AND
 *     nil-platforms after successful unmarshal) must return the same sentinel.
 *
 * @contracts-for-kou
 *   1. In GetCollection, replace:
 *        status, body, err := c.DoGet(ctx, "/api/collection/"+id)
 *      with:
 *        status, body, err := c.DoGet(ctx, "/api/collection/"+url.PathEscape(id))
 *      Add import "net/url".
 *   2. In the status switch, add before or alongside the existing 404 case:
 *        case status == 401, status == 403:
 *            return nil, ErrCollectionNotFound
 *      (Alternatively fold into the 404 case: case status == 401, status == 403, status == 404.)
 *
 * @see ./collection.go
 * @see ../../.yui-soul/reviews/114-techzone-template-agnostic/r1-ei-injection.md (F-01)
 * @see ../../.yui-soul/reviews/114-techzone-template-agnostic/r1-sho-core.md (finding #3)
 * @see ../../.yui-soul/reviews/114-techzone-template-agnostic/r1-shin-tests.md (F-07)
 */

// Package techzone_test — audit remediation RED tests for collection.go.
//
// RED GATE: These tests FAIL against the current collection.go because:
//   (a) GetCollection does NOT url.PathEscape the id — a "/" in the id causes
//       the HTTP path to traverse directories (e.g. /api/collection/a/b routes
//       to a different path). The mock's request recorder exposes the raw path
//       received, which will differ from the expected escaped path.
//   (b) GetCollection does NOT map 401/403 to ErrCollectionNotFound. Both fall
//       through to the JSON decode path and surface as ErrMalformedCollectionResponse
//       (the body from a 401/403 is not a valid collection JSON). The test asserts
//       errors.Is(err, ErrCollectionNotFound), which is currently false.
//   (c) No test existed for syntactically invalid JSON body (F-07).
//
// Goes GREEN when Kou adds url.PathEscape and 401/403 mapping to collection.go.
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
// Contract 3 — collection_id path-escape
// ---------------------------------------------------------------------------

// TestGetCollection_PathEscape_SlashInID verifies that GetCollection url.PathEscape's
// the collection id before building the request path, so that a "/" in the id is
// transmitted as "%2F" and does not cause path traversal.
//
// RED: current GetCollection uses "/api/collection/"+id (plain concatenation).
// A request for id "a/b" results in GET /api/collection/a/b — which the mock
// server routes to a DIFFERENT handler (or 404 on the wrong path prefix), not to
// /api/collection/a%2Fb. The test records the actual received path and asserts
// it equals /api/collection/a%2Fb.
func TestGetCollection_PathEscape_SlashInID(t *testing.T) {
	t.Parallel()

	var receivedPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// r.URL.Path is decoded by net/http — use r.URL.RawPath which preserves
		// the percent-encoded form sent by the client. If RawPath is empty, the
		// path had no encoding needed.
		if r.URL.RawPath != "" {
			receivedPath = r.URL.RawPath
		} else {
			receivedPath = r.URL.Path
		}
		// Return a valid AWS collection so GetCollection doesn't error on content.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(collectionFixtureAWS))
	}))
	t.Cleanup(srv.Close)

	c, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// id with a slash — path-significant character.
	id := "a/b"
	_, _ = c.GetCollection(context.Background(), id)

	// The mock must have received the ESCAPED path, not the raw one.
	// url.PathEscape("a/b") == "a%2Fb"
	wantPath := "/api/collection/a%2Fb"
	if receivedPath != wantPath {
		t.Errorf(
			"FAIL: path-escape contract violated\n"+
				"  received path: %q\n"+
				"  want path:     %q\n"+
				"  GetCollection must apply url.PathEscape(id) before building the request path.\n"+
				"  Without escaping, id %q causes path traversal: /api/collection/%s routes to\n"+
				"  a different endpoint than /api/collection/%s.",
			receivedPath, wantPath, id, id, "a%2Fb",
		)
	}
}

// TestGetCollection_PathEscape_DotDotSlashInID verifies that a "../foo" id is
// also escaped, preventing upward path traversal.
//
// RED: current code sends GET /api/collection/../foo → traversal to /api/foo.
func TestGetCollection_PathEscape_DotDotSlashInID(t *testing.T) {
	t.Parallel()

	var receivedPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawPath != "" {
			receivedPath = r.URL.RawPath
		} else {
			receivedPath = r.URL.Path
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(collectionFixtureAWS))
	}))
	t.Cleanup(srv.Close)

	c, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	id := "../foo"
	_, _ = c.GetCollection(context.Background(), id)

	// url.PathEscape("../foo") == "..%2Ffoo"
	wantPath := "/api/collection/..%2Ffoo"
	if receivedPath != wantPath {
		t.Errorf(
			"FAIL: path-escape contract violated for traversal id\n"+
				"  received path: %q\n"+
				"  want path:     %q\n"+
				"  id %q must be escaped to prevent path traversal.",
			receivedPath, wantPath, id,
		)
	}
}

// TestGetCollection_PathEscape_NormalHexID verifies that a normal 24-char hex
// collection id is transmitted verbatim (url.PathEscape is identity for hex chars).
//
// This is the counter-case — ensures the escape does not corrupt normal IDs.
// Should remain GREEN before and after the fix.
func TestGetCollection_PathEscape_NormalHexID(t *testing.T) {
	t.Parallel()

	var receivedPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawPath != "" {
			receivedPath = r.URL.RawPath
		} else {
			receivedPath = r.URL.Path
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(collectionFixtureAWS))
	}))
	t.Cleanup(srv.Close)

	c, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	id := "69650af0758b9e41de66b6ae" // normal hex MongoDB ObjectID
	_, _ = c.GetCollection(context.Background(), id)

	wantPath := "/api/collection/69650af0758b9e41de66b6ae"
	if receivedPath != wantPath {
		t.Errorf(
			"FAIL: normal hex id must be transmitted verbatim\n"+
				"  received path: %q\n"+
				"  want path:     %q",
			receivedPath, wantPath,
		)
	}
}

// ---------------------------------------------------------------------------
// Contract 4 — 401/403 mapping to ErrCollectionNotFound
// ---------------------------------------------------------------------------

// TestGetCollection_401_ReturnsErrCollectionNotFound verifies that HTTP 401
// maps to ErrCollectionNotFound, NOT ErrMalformedCollectionResponse.
//
// RED: current GetCollection has no 401 case in its status switch. A 401 response
// falls through to the JSON decode path. The auth-rejection body (e.g. HTML or a
// small error JSON without "platforms") causes json.Unmarshal to fail or envelope.Platforms
// to be nil → ErrMalformedCollectionResponse is returned. The test asserts
// errors.Is(err, ErrCollectionNotFound) == true, which is currently FALSE.
func TestGetCollection_401_ReturnsErrCollectionNotFound(t *testing.T) {
	t.Parallel()

	srv := newCollectionServer(t, http.StatusUnauthorized, `{"error":"Unauthorized","message":"Token is expired or invalid"}`)
	c := newCollectionClient(t, srv)

	_, err := c.GetCollection(context.Background(), "69650af0758b9e41de66b6ae")
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}

	// Must map to ErrCollectionNotFound (access-gated = not found for caller).
	if !errors.Is(err, techzone.ErrCollectionNotFound) {
		t.Errorf(
			"FAIL: 401 must map to ErrCollectionNotFound (access-gated)\n"+
				"  errors.Is(err, ErrCollectionNotFound) == false\n"+
				"  got: %v\n"+
				"  GetCollection must handle 401 the same as 404: access denied == not found.",
			err,
		)
	}

	// Must NOT be mistakenly surfaced as ErrMalformedCollectionResponse.
	if errors.Is(err, techzone.ErrMalformedCollectionResponse) {
		t.Errorf(
			"FAIL: 401 must NOT return ErrMalformedCollectionResponse\n"+
				"  A 401 is an auth error, not a malformed response.\n"+
				"  Current behavior: 401 falls through to JSON decode, auth body is non-collection\n"+
				"  JSON → ErrMalformedCollectionResponse. This mislabels the error.",
		)
	}

	assertNoTokenLeak(t, err.Error())
}

// TestGetCollection_403_ReturnsErrCollectionNotFound verifies that HTTP 403
// maps to ErrCollectionNotFound, NOT ErrMalformedCollectionResponse.
//
// RED: same rationale as 401 test above.
func TestGetCollection_403_ReturnsErrCollectionNotFound(t *testing.T) {
	t.Parallel()

	srv := newCollectionServer(t, http.StatusForbidden, `{"error":"Forbidden","message":"Insufficient permissions"}`)
	c := newCollectionClient(t, srv)

	_, err := c.GetCollection(context.Background(), "69650af0758b9e41de66b6ae")
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}

	if !errors.Is(err, techzone.ErrCollectionNotFound) {
		t.Errorf(
			"FAIL: 403 must map to ErrCollectionNotFound (access-gated)\n"+
				"  errors.Is(err, ErrCollectionNotFound) == false\n"+
				"  got: %v",
			err,
		)
	}

	if errors.Is(err, techzone.ErrMalformedCollectionResponse) {
		t.Errorf("FAIL: 403 must NOT return ErrMalformedCollectionResponse")
	}

	assertNoTokenLeak(t, err.Error())
}

// ---------------------------------------------------------------------------
// Contract 8 — malformed body: syntactically invalid JSON
// ---------------------------------------------------------------------------

// TestGetCollection_InvalidJSONBody_ReturnsErrMalformedCollectionResponse
// verifies that a 200 response with a body that is NOT valid JSON (parse error
// on json.Unmarshal) returns ErrMalformedCollectionResponse.
//
// This exercises the FIRST branch in GetCollection's 200-path:
//   if err := json.Unmarshal(body, &envelope); err != nil { ... ErrMalformedCollectionResponse }
//
// The existing TestGetCollection_MalformedJSON_ReturnsErrMalformedCollectionResponse
// covers only the case where the body IS valid JSON but lacks the "platforms" key.
// This test covers the syntactically-broken-JSON path (Shin audit F-07).
//
// This test should be GREEN against current code (the json.Unmarshal branch already
// exists and returns ErrMalformedCollectionResponse). It is added for explicit
// coverage of the previously-untested path.
func TestGetCollection_InvalidJSONBody_ReturnsErrMalformedCollectionResponse(t *testing.T) {
	t.Parallel()

	invalidBodies := []struct {
		name string
		body string
	}{
		{"plain_text", "this is not json"},
		{"truncated_object", `{"platforms": [`},
		{"bare_word", `undefined`},
		{"html_body", `<html><body>Sign in to IBM</body></html>`},
	}

	for _, tc := range invalidBodies {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := newCollectionServer(t, http.StatusOK, tc.body)
			c := newCollectionClient(t, srv)

			_, err := c.GetCollection(context.Background(), "any-id")
			if err == nil {
				t.Fatalf("FAIL: expected error for invalid JSON body %q, got nil", tc.body)
			}

			if !errors.Is(err, techzone.ErrMalformedCollectionResponse) {
				t.Errorf(
					"FAIL: invalid JSON body must return ErrMalformedCollectionResponse\n"+
						"  body: %q\n"+
						"  errors.Is(err, ErrMalformedCollectionResponse) == false\n"+
						"  got: %v",
					tc.body, err,
				)
			}

			assertNoTokenLeak(t, err.Error())
		})
	}
}

// ---------------------------------------------------------------------------
// Token-safety helper — reused from collection_test.go (same package)
// ---------------------------------------------------------------------------

// assertNoTokenLeak is defined in collection_test.go in this package.
// The sentinel is defined in client_test.go.
// Both are in package techzone_test — no re-declaration needed.
