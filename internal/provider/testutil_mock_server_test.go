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

	// pollResponseSequence: when non-nil, handlePoll uses sequence-based responses.
	//   - For the nth poll (0-indexed), returns pollResponseSequence[n] as the body.
	//   - When the index exceeds the slice length, the last element is repeated.
	//   - An empty string "" represents a nil-status body: `{}`.
	//   - A non-empty string is used verbatim as the JSON body.
	// When nil (default), handlePoll uses the readyAfterPollCount/pollShouldFail logic.
	pollResponseSequence []string

	// pollDone: set to true once the poll loop has received a terminal status
	// (Ready, Failed, or a non-retrying sequence end). After this, GET /api/reservation/aws/<id>
	// calls are canonical Reads (not polls) and must go to handleCanonicalRead.
	pollDone bool

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

	// --- Poll legacy path override (GET /api/reservation/<id>) ---

	// pollLegacyMode controls the response for GET /api/reservation/<id>
	// (the legacy poll path, distinct from /api/reservation/aws/<id>):
	//   ""             — normal behaviour: use readyAfterPollCount / pollShouldFail / pollResponseSequence (default)
	//   "302_redirect" — always return HTTP 302 with Location: /api/reservation/unknown/<id>,
	//                    mimicking the real TechZone API that permanently redirects the legacy
	//                    endpoint to an "unknown" path. The provider's HTTP client does NOT
	//                    follow redirects (CheckRedirect = ErrUseLastResponse), so it receives
	//                    the 302 raw. The poll loop treats non-2xx as "retry", looping forever.
	//                    This mode drives TestAccReservation_Poll_LegacyEndpointReturns302_MustNotHang.
	pollLegacyMode string

	// --- Typed poll path (GET /api/reservation/aws/<id>) — poll sequence ---

	// awsPollResponseSequence is a parallel sequence used when pollLegacyMode is
	// set to "302_redirect" and the fix routes polls to /api/reservation/aws/<id>.
	// The nth GET /api/reservation/aws/<id> (0-indexed) returns awsPollResponseSequence[n].
	// When the index exceeds the slice, the last element is repeated.
	// Empty string "" → `{}` (nil-status); non-empty → verbatim JSON.
	// When nil AND pollLegacyMode=="302_redirect", the canonical handleCanonicalRead
	// handler services /aws/<id> — which defaults to "ready" mode.
	awsPollResponseSequence []string
	awsPollCount            int // tracks GET /api/reservation/aws/<id> calls

	// --- Request recording (for assertions) ---

	// DeleteBodies stores the raw request bodies of each DELETE call.
	DeleteBodies [][]byte
	// AuthHeaders stores the raw Authorization header value from each request.
	AuthHeaders []string
	// PollURLLog records the URL path of every GET that hits either the legacy
	// poll path (/api/reservation/<id>) or the typed poll path
	// (/api/reservation/aws/<id>).  Used to assert which endpoint the poll
	// loop actually used.
	PollURLLog []string
}

// mockCollectionResponse holds the canned HTTP response for a collection ID.
type mockCollectionResponse struct {
	statusCode int
	body       string
}

// testCollectionID is the canonical collection ID used across all acceptance tests.
// It must be a valid 24-character hexadecimal string (MongoDB ObjectID format) to
// pass schema validation (^[a-fA-F0-9]{24}$) at plan time.
// Value chosen to match the canonical ID already used in payload_golden_test.go.
const testCollectionID = "69650af0758b9e41de66b6ae"

// ddrCollectionJSONDefault is the default DDR collection response used by
// newMockServer to pre-seed testCollectionID so that all existing
// acceptance tests (which use reservationConfig / reservationConfigWithTimeout)
// get a valid AWS collection without requiring per-test SetCollectionResponse calls.
//
// The canonical definition of this fixture lives in resource_reservation_create_wired_test.go
// (ddrCollectionJSON); this copy keeps the mock server self-contained.
const ddrCollectionJSONDefault = `{
  "id": "69650af0758b9e41de66b6ae",
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
// The mock is pre-seeded with a DDR AWS collection response for testCollectionID
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
			testCollectionID: {
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

// SetPollResponseSequence installs a poll response sequence.
// When set (non-nil), handlePoll uses the nth element for the nth poll call
// (0-indexed). When the index exceeds the slice length, the last element is
// repeated. An empty string "" causes the response body to be `{}` (no status
// field), exercising the nil-status retry path in the poll loop.
// Call before the test step that triggers Create.
func (m *mockTechZoneServer) SetPollResponseSequence(seq []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pollResponseSequence = seq
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
	// Record GET paths for both poll endpoints so tests can assert which one was used.
	if r.Method == http.MethodGet &&
		(strings.HasPrefix(r.URL.Path, "/api/reservation/aws/") ||
			(strings.HasPrefix(r.URL.Path, "/api/reservation/") &&
				!strings.HasPrefix(r.URL.Path, "/api/reservation/aws"))) {
		m.PollURLLog = append(m.PollURLLog, r.URL.Path)
	}
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
		m.handleCanonicalReadOrAwsPoll(w, r)

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

// handlePoll — GET /api/reservation/<id>  (legacy status poll, not the aws/<id> path)
//
// When pollLegacyMode == "302_redirect", this handler mimics the real TechZone API
// behaviour: the legacy /api/reservation/<id> endpoint PERMANENTLY redirects to
// /api/reservation/unknown/<id>. The provider's HTTP client does not follow redirects
// (CheckRedirect = ErrUseLastResponse), so it receives the 302 raw.  The poll loop
// then sees a non-2xx status and retries — forever, because the redirect is permanent.
//
// This mode is set by TestAccReservation_Poll_LegacyEndpointReturns302_MustNotHang to
// drive the regression test (RED gate): the bug in the current code.
func (m *mockTechZoneServer) handlePoll(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	legacyMode := m.pollLegacyMode
	count := m.pollCount
	m.pollCount++
	readyAfter := m.readyAfterPollCount
	fail := m.pollShouldFail
	seq := m.pollResponseSequence
	m.mu.Unlock()

	// 302-redirect mode: always redirect the legacy endpoint to unknown/<id>.
	// This is the exact behaviour of the real TechZone API that caused the
	// production hang (root cause from terraform apply log).
	if legacyMode == "302_redirect" {
		// Extract the reservation ID from the path: /api/reservation/<id>
		id := strings.TrimPrefix(r.URL.Path, "/api/reservation/")
		w.Header().Set("Location", "/api/reservation/unknown/"+id)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusFound) // 302
		fmt.Fprintf(w, "<html><body>Found. Redirecting to /api/reservation/unknown/%s</body></html>", id)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	// Sequence-based mode: when pollResponseSequence is set, use it.
	// This exercises paths the simple readyAfterPollCount logic cannot reach
	// (e.g. nil-status responses that trigger warn+continue in the poll loop).
	if seq != nil {
		idx := count
		if idx >= len(seq) {
			idx = len(seq) - 1
		}
		body := seq[idx]
		if body == "" {
			// An empty string in the sequence means "nil-status body": no status field.
			fmt.Fprint(w, `{}`)
		} else {
			fmt.Fprint(w, body)
		}
		return
	}

	// Default mode: readyAfterPollCount / pollShouldFail logic (backwards compatible).
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

// handleCanonicalReadOrAwsPoll — GET /api/reservation/aws/<id>
//
// This handler serves two roles:
//   (a) poll requests from the provider's poll loop (the fixed endpoint —
//       /api/reservation/aws/<id> has no legacy 302 redirect), and
//   (b) the canonical Read after polling completes (Terraform Read/Destroy calls).
//
// Dispatch priority:
//  1. pollDone == true → poll finished; this is a canonical Read — delegate to handleCanonicalRead.
//  2. awsPollResponseSequence set → explicit per-aws-endpoint sequence (overrides all poll logic).
//  3. pollResponseSequence set → shared poll sequence (exercises nil-status retry path).
//  4. pollShouldFail / readyAfterPollCount set → standard Provisioning→Failed/Ready progression.
//  5. None of the above → fast-path: return Ready immediately (no poll scenario configured).
//
// pollDone is set once a terminal poll body is dispatched (Ready, Failed, or exhausted sequence).
// After that, all /aws/<id> requests are canonical Reads.
//
// Rules (3) and (4) share pollCount with handlePoll so existing tests that configure
// readyAfterPollCount / pollShouldFail / pollResponseSequence work without change after
// the poll loop was moved from /api/reservation/<id> to /api/reservation/aws/<id>.
func (m *mockTechZoneServer) handleCanonicalReadOrAwsPoll(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()

	// (1) Once polling is done, all /aws/<id> calls are canonical Reads.
	if m.pollDone {
		m.mu.Unlock()
		m.handleCanonicalRead(w, r)
		return
	}

	awsSeq := m.awsPollResponseSequence
	awsCount := m.awsPollCount
	if awsSeq != nil {
		m.awsPollCount++
	}

	pollSeq := m.pollResponseSequence
	pollCount := m.pollCount
	readyAfter := m.readyAfterPollCount
	fail := m.pollShouldFail

	// Increment pollCount for non-awsSeq paths so readyAfterPollCount and
	// pollResponseSequence behave identically to when handlePoll served the request.
	usePollLogic := awsSeq == nil && (pollSeq != nil || fail || readyAfter > 0)
	if usePollLogic {
		m.pollCount++
	}
	m.mu.Unlock()

	// (2) awsPollResponseSequence: explicit per-aws-endpoint sequence.
	if awsSeq != nil {
		idx := awsCount
		if idx >= len(awsSeq) {
			idx = len(awsSeq) - 1
		}
		body := awsSeq[idx]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if body == "" {
			fmt.Fprint(w, `{}`)
		} else {
			fmt.Fprint(w, body)
		}
		// Mark poll done when the sequence is exhausted or returns a terminal status.
		// For simplicity: mark done after any non-empty body (caller controls the sequence).
		if body != "" && body != `{"status":"Provisioning"}` {
			m.mu.Lock()
			m.pollDone = true
			m.mu.Unlock()
		}
		return
	}

	// (3) pollResponseSequence: shared sequence (nil-status retry tests).
	if pollSeq != nil {
		idx := pollCount
		if idx >= len(pollSeq) {
			idx = len(pollSeq) - 1
		}
		body := pollSeq[idx]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if body == "" {
			fmt.Fprint(w, `{}`)
		} else {
			fmt.Fprint(w, body)
			// Terminal body dispatched: subsequent /aws/<id> calls are canonical Reads.
			if body != `{"status":"Provisioning"}` {
				m.mu.Lock()
				m.pollDone = true
				m.mu.Unlock()
			}
		}
		return
	}

	// (4) pollShouldFail / readyAfterPollCount: standard Provisioning→Failed/Ready progression.
	if fail || readyAfter > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if pollCount < readyAfter {
			fmt.Fprint(w, `{"status":"Provisioning"}`)
			return
		}
		// Terminal status: mark poll done before returning.
		m.mu.Lock()
		m.pollDone = true
		m.mu.Unlock()
		if fail {
			fmt.Fprint(w, `{"status":"Failed"}`)
			return
		}
		fmt.Fprint(w, `{"status":"Ready"}`)
		return
	}

	// (5) Default: no poll scenario configured — return Ready immediately and
	// mark poll done so subsequent canonical Reads go to handleCanonicalRead.
	m.mu.Lock()
	m.pollDone = true
	m.mu.Unlock()
	m.handleCanonicalRead(w, r)
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
// ibmtechzone_reservation resource, using the given mock server URL and api_key.
//
// E5 update: hcp_org / hcp_project removed from schema; replaced by template_variables map.
func reservationConfig(mockURL, apiKey string) string {
	return fmt.Sprintf(`
provider "ibmtechzone" {
  api_key  = %q
  api_base = %q
}

resource "ibmtechzone_reservation" "test" {
  collection_id             = "69650af0758b9e41de66b6ae"
  user_email                = "test@example.com"
  template_variables        = {
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
// E5 update: hcp_org / hcp_project removed; replaced by template_variables map.
func reservationConfigWithTimeout(mockURL, apiKey string, timeoutMinutes int) string {
	return fmt.Sprintf(`
provider "ibmtechzone" {
  api_key  = %q
  api_base = %q
}

resource "ibmtechzone_reservation" "test" {
  collection_id             = "69650af0758b9e41de66b6ae"
  user_email                = "test@example.com"
  template_variables        = {
    "_04_hcp_org"     = "test-hcp-org"
    "_05_hcp_project" = "test-hcp-project"
  }
  timeout_minutes           = %d
  reservation_duration_days = 1
}
`, apiKey, mockURL, timeoutMinutes)
}
