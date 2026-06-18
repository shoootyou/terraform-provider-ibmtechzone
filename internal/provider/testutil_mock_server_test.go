// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

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
// It handles all four endpoint groups used by the provider:
//
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

	// --- Create (POST /api/reservation/aws) ---

	// createShouldFail: when true, POST returns 500 instead of 200+id.
	createShouldFail bool

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
	//   "past_expiry" — 200 + Ready + provisionUntil 1 hour ago
	canonicalReadMode string

	// --- Delete (DELETE /api/reservation/aws/<id>) ---

	// deleteStatusSequence: successive DELETE calls return these HTTP status codes.
	// When exhausted, the last code is repeated. Default is [200].
	deleteStatusSequence []int
	deleteCallCount      int

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

// newMockServer creates and starts a new mockTechZoneServer.
// The server is automatically closed when the test ends.
func newMockServer(t *testing.T) *mockTechZoneServer {
	t.Helper()
	m := &mockTechZoneServer{
		t:                    t,
		canonicalReadMode:    "ready",
		tokenValidateMode:    "valid",
		deleteStatusSequence: []int{200},
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.ServeHTTP))
	t.Cleanup(m.server.Close)
	return m
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

// handleCreate — POST /api/reservation/aws
func (m *mockTechZoneServer) handleCreate(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	fail := m.createShouldFail
	m.mu.Unlock()

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
	idx := m.deleteCallCount
	if idx >= len(m.deleteStatusSequence) {
		idx = len(m.deleteStatusSequence) - 1
	}
	code := m.deleteStatusSequence[idx]
	m.deleteCallCount++

	// Record body for assertions.
	body := make([]byte, 0)
	if r.Body != nil {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		body = buf[:n]
	}
	m.DeleteBodies = append(m.DeleteBodies, body)
	m.mu.Unlock()

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
func reservationConfig(mockURL, apiKey string) string {
	return fmt.Sprintf(`
provider "techzone" {
  api_key  = %q
  api_base = %q
}

resource "techzone_reservation" "test" {
  collection_id             = "test-collection-id"
  user_email                = "test@example.com"
  hcp_org                   = "test-hcp-org"
  hcp_project               = "test-hcp-project"
  timeout_minutes           = 1
  reservation_duration_days = 1
}
`, apiKey, mockURL)
}
