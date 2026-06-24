package provider_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// mockTechZoneServer — in-process httptest.Server simulating TechZone API
// ---------------------------------------------------------------------------

// mockTechZoneServer is a configurable in-process mock of the TechZone API.
// It handles all five endpoint groups used by the provider:
//
//	GET  /api/collection/<id>                    — collection fetch (E5)
//	POST /api/reservation/aws                    — create
//	GET  /api/reservation/<id>                   — poll (status only)
//	GET  /api/reservation/aws/<id>               — canonical read
//	DELETE /api/reservation/aws/<id>             — delete
//	GET  /api/my/reservations/all                — token validate (Configure probe)
//
// All server methods are goroutine-safe via mu.
type mockTechZoneServer struct {
	t      *testing.T
	server *httptest.Server

	mu sync.Mutex

	// --- Collection fetch (GET /api/collection/<id>) — E5 addition ---

	// collectionResponses maps collection ID → {statusCode, body}.
	// If the requested ID is not in the map, returns 404.
	collectionResponses map[string]mockCollectionResponse

	// --- Create (POST /api/reservation/aws) ---

	// createShouldFail: when true, POST returns 500 instead of 200+id.
	createShouldFail bool

	// createBodyCapture: if non-nil, called with the raw POST body on each create.
	// Goroutine-safe: called under mu.
	createBodyCapture func([]byte)

	// createCallCount: number of times POST /api/reservation/aws was called.
	createCallCount int

	// --- Poll (GET /api/reservation/<id>) ---

	// readyAfterPollCount: the poll endpoint returns "Provisioning" for this
	// many requests, then "Ready" on the next. 0 = Ready on the very first poll.
	readyAfterPollCount int
	pollCount           int // tracks how many poll requests have been served

	// pollShouldFail: when true, the first non-Provisioning poll returns "Failed"
	// instead of "Ready".
	pollShouldFail bool

	// --- Canonical read (GET /api/reservation/aws/<id>) ---

	// canonicalReadMode controls what GET /api/reservation/aws/<id> returns:
	//   "ready"       — 200 + Ready response with serviceLinks (default)
	//   "404"         — 404
	//   "deleted"     — 200 + status=Deleted
	//   "expired"     — 200 + status=Expired (terminal, not 404)
	//   "past_expiry" — 200 + Ready + provisionUntil 1 hour ago
	canonicalReadMode string

	// --- Delete (DELETE /api/reservation/aws/<id>) ---

	// deleteStatusSequence: successive DELETE calls return these HTTP status codes.
	// When exhausted, the last code is repeated. Default is [200].
	deleteStatusSequence []int
	deleteCallCount      int

	// deleteMode overrides deleteStatusSequence for auth-failure scenarios:
	//   ""             — use deleteStatusSequence (default)
	//   "302_redirect" — 302 with Location: /sso (SSO redirect on expired token)
	//   "401"          — 401 Unauthorized (auth failure)
	//   "403"          — 403 Forbidden (auth failure)
	//   "500"          — 500 Internal Server Error (non-auth failure)
	deleteMode string

	// --- Token validate (GET /api/my/reservations/all) ---

	// tokenValidateMode controls what GET /api/my/reservations/all returns:
	//   "valid"   — 200 + JSON []  (default)
	//   "html"    — 200 + HTML (SSO page)
	//   "401"     — 401
	//   "redirect"— 302
	tokenValidateMode string

	// --- Request recording (for assertions) ---

	// DeleteBodies stores the raw request bodies of each DELETE call.
	DeleteBodies [][]byte
	// AuthHeaders stores the raw Authorization header value from each request.
	AuthHeaders []string
}

// mockCollectionResponse holds the canned HTTP response for a collection ID.
type mockCollectionResponse struct {
	statusCode int
	body       string
}

// ddrCollectionJSONDefault is the default DDR collection response used by
// newMockServer to pre-seed "test-collection-id" so that all existing
// acceptance tests (which use reservationConfig / reservationConfigWithTimeout)
// get a valid AWS collection without requiring per-test SetCollectionResponse calls.
//
// The canonical definition of this fixture lives in resource_reservation_create_wired_test.go
// (ddrCollectionJSON); this copy keeps the mock server self-contained.
const ddrCollectionJSONDefault = `{
  "id": "test-collection-id",
  "platforms": [
    {
      "oid": "62ccb18c2d38520017eec9fb",
      "id": "69651c138d6e497dc77a8dbe",
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

// newMockServer creates and starts a new mockTechZoneServer.
// The server is automatically closed when the test ends.
//
// The mock is pre-seeded with a DDR AWS collection response for "test-collection-id"
// so that all tests using reservationConfig / reservationConfigWithTimeout get a
// valid collection without requiring explicit SetCollectionResponse calls.
func newMockServer(t *testing.T) *mockTechZoneServer {
	t.Helper()
	m := &mockTechZoneServer{
		t:                    t,
		canonicalReadMode:    "ready",
		tokenValidateMode:    "valid",
		deleteStatusSequence: []int{200},
		collectionResponses: map[string]mockCollectionResponse{
			"test-collection-id": {
				statusCode: 200,
				body:       ddrCollectionJSONDefault,
			},
		},
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.ServeHTTP))
	t.Cleanup(m.server.Close)
	return m
}

// SetCollectionResponse registers a canned response for GET /api/collection/<id>.
// The server returns statusCode and body for requests matching that collection ID.
// Call before the test step that triggers Create.
func (m *mockTechZoneServer) SetCollectionResponse(collectionID string, statusCode int, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.collectionResponses == nil {
		m.collectionResponses = make(map[string]mockCollectionResponse)
	}
	m.collectionResponses[collectionID] = mockCollectionResponse{
		statusCode: statusCode,
		body:       body,
	}
}

// SetCreateBodyCapture installs a callback that receives the raw POST body of
// each POST /api/reservation/aws call. Called under m.mu — the callback must
// not call any m.* methods to avoid deadlock.
func (m *mockTechZoneServer) SetCreateBodyCapture(fn func([]byte)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createBodyCapture = fn
}

// URL returns the base URL of the mock server (e.g. "http://127.0.0.1:PORT").
func (m *mockTechZoneServer) URL() string {
	return m.server.URL
}

// Close shuts down the mock server.
func (m *mockTechZoneServer) Close() {
	m.server.Close()
}

// ServeHTTP dispatches requests to the appropriate handler.
func (m *mockTechZoneServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.AuthHeaders = append(m.AuthHeaders, r.Header.Get("Authorization"))
	m.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/my/reservations/all":
		m.handleTokenValidate(w, r)

	// Collection fetch MUST be matched before /api/reservation/ prefix checks
	// because /api/collection/ is a different path prefix entirely.
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/collection/"):
		m.handleCollection(w, r)

	case r.Method == http.MethodPost && r.URL.Path == "/api/reservation/aws":
		m.handleCreate(w, r)

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/reservation/aws/"):
		m.handleCanonicalRead(w, r)

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/reservation/"):
		m.handlePoll(w, r)

	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/reservation/aws/"):
		m.handleDelete(w, r)

	default:
		m.t.Logf("mock: unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

// handleTokenValidate — GET /api/my/reservations/all
func (m *mockTechZoneServer) handleTokenValidate(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	mode := m.tokenValidateMode
	m.mu.Unlock()

	switch mode {
	case "html":
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "<html><body>Sign in to IBM</body></html>")
	case "401":
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"Unauthorized"}`)
	case "redirect":
		w.Header().Set("Location", "/sso-login")
		w.WriteHeader(http.StatusMovedPermanently)
	default: // "valid"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[]`)
	}
}

// handleCollection — GET /api/collection/<id>
//
// Returns a canned response registered via SetCollectionResponse.
// If no response is registered for the given ID, returns 404.
func (m *mockTechZoneServer) handleCollection(w http.ResponseWriter, r *http.Request) {
	// Extract the collection ID from the path: /api/collection/<id>
	id := strings.TrimPrefix(r.URL.Path, "/api/collection/")

	m.mu.Lock()
	resp, ok := m.collectionResponses[id]
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"collection not found"}`)
		return
	}
	w.WriteHeader(resp.statusCode)
	fmt.Fprint(w, resp.body)
}

// handleCreate — POST /api/reservation/aws
func (m *mockTechZoneServer) handleCreate(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	fail := m.createShouldFail
	capture := m.createBodyCapture
	m.createCallCount++
	m.mu.Unlock()

	// Capture the body if a capture callback is registered.
	// Body can only be read once, so we always drain it here.
	if capture != nil {
		var bodyBytes []byte
		if r.Body != nil {
			buf := make([]byte, 1<<20) // 1 MiB — more than enough for any reservation payload
			n, _ := r.Body.Read(buf)
			bodyBytes = buf[:n]
		}
		m.mu.Lock()
		capture(bodyBytes)
		m.mu.Unlock()
	}

	if fail {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"internal server error"}`)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"id":"test-reservation-id"}`)
}

// handlePoll — GET /api/reservation/<id>  (status poll, not the aws/<id> path)
func (m *mockTechZoneServer) handlePoll(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	count := m.pollCount
	m.pollCount++
	readyAfter := m.readyAfterPollCount
	fail := m.pollShouldFail
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if count < readyAfter {
		fmt.Fprint(w, `{"status":"Provisioning"}`)
		return
	}
	if fail {
		fmt.Fprint(w, `{"status":"Failed"}`)
		return
	}
	fmt.Fprint(w, `{"status":"Ready"}`)
}

// canonicalReadyBody is the standard 200+Ready response body with serviceLinks.
func canonicalReadyBody() string {
	return `{
		"id": "test-reservation-id",
		"status": "Ready",
		"serviceLinks": [
			{"type": "AWS Console", "url": "https://console.aws.amazon.com/test"}
		],
		"provisionDate":  "2024-06-15T10:00:00Z",
		"provisionUntil": "2099-12-31T23:59:59Z",
		"start":     null,
		"end":       null,
		"startDate": null,
		"endDate":   null
	}`
}

// handleCanonicalRead — GET /api/reservation/aws/<id>
func (m *mockTechZoneServer) handleCanonicalRead(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	mode := m.canonicalReadMode
	m.mu.Unlock()

	switch mode {
	case "404":
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"not found"}`)

	case "deleted":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// gotchas/techzone.md: expired/reclaimed reservations return 200 + status=Deleted
		fmt.Fprint(w, `{
			"id": "test-reservation-id",
			"status": "Deleted",
			"serviceLinks": [{"type": "AWS Console", "url": "https://stale.example.com"}],
			"provisionDate":  null,
			"provisionUntil": null
		}`)

	case "expired":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// TechZone may return 200 + status=Expired for a reservation that has
		// passed its window. IsTerminalStatus("Expired") → true → RemoveResource().
		fmt.Fprint(w, `{
			"id": "test-reservation-id",
			"status": "Expired",
			"serviceLinks": [{"type": "AWS Console", "url": "https://stale.example.com"}],
			"provisionDate":  null,
			"provisionUntil": null
		}`)

	case "past_expiry":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// provisionUntil is 1 hour in the past → PastExpiry = true → prune
		pastTime := time.Now().UTC().Add(-1 * time.Hour).Format("2006-01-02T15:04:05Z")
		body, _ := json.Marshal(map[string]any{
			"id":             "test-reservation-id",
			"status":         "Ready",
			"serviceLinks":   []any{},
			"provisionDate":  nil,
			"provisionUntil": pastTime,
		})
		w.Write(body) //nolint:errcheck

	default: // "ready"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, canonicalReadyBody())
	}
}

// handleDelete — DELETE /api/reservation/aws/<id>
func (m *mockTechZoneServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()

	// Record body for assertions.
	body := make([]byte, 0)
	if r.Body != nil {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		body = buf[:n]
	}
	m.DeleteBodies = append(m.DeleteBodies, body)

	mode := m.deleteMode
	idx := m.deleteCallCount
	if idx >= len(m.deleteStatusSequence) {
		idx = len(m.deleteStatusSequence) - 1
	}
	code := m.deleteStatusSequence[idx]
	m.deleteCallCount++
	m.mu.Unlock()

	// Auth-failure modes (override deleteStatusSequence).
	// These simulate a TechZone API returning an auth error on DELETE —
	// which happens when the token has expired mid-operation.
	switch mode {
	case "302_redirect":
		// 302 with SSO Location header — exactly what TechZone returns when the
		// Bearer token is expired. The client has CheckRedirect=ErrUseLastResponse
		// so the redirect is observed raw (status=302) rather than followed.
		w.Header().Set("Location", m.server.URL+"/sso-login")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusFound)
		fmt.Fprint(w, "<html><body>Sign in to IBM</body></html>")
		return
	case "401":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"Unauthorized","message":"Token is expired or invalid"}`)
		return
	case "403":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":"Forbidden","message":"Insufficient permissions"}`)
		return
	}

	// Default: use deleteStatusSequence.
	if code == 500 {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"delete failed"}`)
		return
	}
	w.WriteHeader(code)
}

// ---------------------------------------------------------------------------
// Shared HCL config helpers for acceptance tests
// ---------------------------------------------------------------------------

// reservationConfig returns a complete Terraform config for a
// techzone_reservation resource, using the given mock server URL and api_key.
//
// E5 update: hcp_org / hcp_project removed from schema; replaced by dynamic_outputs map.
func reservationConfig(mockURL, apiKey string) string {
	return fmt.Sprintf(`
provider "techzone" {
  api_key  = %q
  api_base = %q
}

resource "techzone_reservation" "test" {
  collection_id             = "test-collection-id"
  user_email                = "test@example.com"
  dynamic_outputs           = {
    "_04_hcp_org"     = "test-hcp-org"
    "_05_hcp_project" = "test-hcp-project"
  }
  timeout_minutes           = 1
  reservation_duration_days = 1
}
`, apiKey, mockURL)
}

// reservationConfigWithTimeout returns a config identical to reservationConfig
// but with an explicit timeout_minutes value so tests can change it between steps.
//
// E5 update: hcp_org / hcp_project removed; replaced by dynamic_outputs map.
func reservationConfigWithTimeout(mockURL, apiKey string, timeoutMinutes int) string {
	return fmt.Sprintf(`
provider "techzone" {
  api_key  = %q
  api_base = %q
}

resource "techzone_reservation" "test" {
  collection_id             = "test-collection-id"
  user_email                = "test@example.com"
  dynamic_outputs           = {
    "_04_hcp_org"     = "test-hcp-org"
    "_05_hcp_project" = "test-hcp-project"
  }
  timeout_minutes           = %d
  reservation_duration_days = 1
}
`, apiKey, mockURL, timeoutMinutes)
}
