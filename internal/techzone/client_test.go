/**
 * @spec-handoff
 *
 * @interface NewClient(apiBase string, apiKey string, opts ...func(*Client)) (*Client, error)
 *
 * @behavior
 *   - Accepts any https:// base URL without error.
 *   - Accepts http:// when the host is exactly 127.0.0.1, localhost, or ::1
 *     (loopback-aware guard matching validate-token.sh — enables httptest.Server in tests).
 *   - Rejects any other non-https base (e.g. http://evil.com) with a non-nil error
 *     whose message MUST NOT contain the sentinel api_key value.
 *   - The returned *http.Client has CheckRedirect set so that it returns
 *     http.ErrUseLastResponse for every redirect — i.e. 3xx responses are NEVER
 *     followed and surface as-is to the caller.
 *   - Overall HTTP client timeout is 30 seconds.
 *
 * @interface (*Client).DoGet(ctx context.Context, path string) (status int, body []byte, err error)
 *
 * @behavior
 *   - Sets the Authorization: Bearer <api_key> request header — NEVER puts the key
 *     in a URL query param or logs/returns it in any error/diagnostic string.
 *   - Returns (status, body, nil) on any completed HTTP response (including 4xx/5xx).
 *   - Returns (0, nil, err) on transport failure (connection refused, DNS failure, etc.).
 *   - The sentinel api_key value MUST NOT appear in any returned error string.
 *
 * @edge-cases
 *   - A 302 response from the server is returned as status=302 (not followed):
 *     this is the "expired token → SSO redirect" signal; status-wins in the caller.
 *   - A transport error (unreachable server) returns a connectivity-class error, not
 *     a token-blame error.
 *   - The api_key sentinel value is absent from all error strings on every failure path.
 *
 * @see ./client.go
 * @see ../provider/provider.go  (ValidateConfig / Configure consume this client)
 */

package techzone_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shoootyou-ext/terraform-provider-ibmtechzone/internal/techzone"
)

// sentinelToken is used across all tests to verify the api_key value never leaks
// into error messages or diagnostics (RFC §4 / Ei F-02 / Shin F-5).
const sentinelToken = "SENTINEL-TOKEN-DO-NOT-LOG"

// assertNoTokenLeak fails the test if the sentinel token appears in msg.
// Every error/diagnostic string produced by the client or provider must pass this check.
func assertNoTokenLeak(t *testing.T, msg string) {
	t.Helper()
	if strings.Contains(msg, sentinelToken) {
		t.Errorf("token safety violation: sentinel token found in message: %q", msg)
	}
}

// ---------------------------------------------------------------------------
// NewClient — base URL guard
// ---------------------------------------------------------------------------

func TestNewClient_AcceptsHTTPS(t *testing.T) {
	t.Parallel()
	c, err := techzone.NewClient("https://api.techzone.ibm.com", sentinelToken)
	if err != nil {
		t.Fatalf("expected no error for https base, got: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil *Client for valid https base")
	}
}

func TestNewClient_AcceptsHTTP_Loopback_127(t *testing.T) {
	t.Parallel()
	// httptest.Server uses 127.0.0.1; this validates the loopback exemption.
	c, err := techzone.NewClient("http://127.0.0.1:8795", sentinelToken)
	if err != nil {
		t.Fatalf("expected no error for loopback http base (127.0.0.1), got: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil *Client for loopback http base")
	}
}

func TestNewClient_AcceptsHTTP_Loopback_localhost(t *testing.T) {
	t.Parallel()
	c, err := techzone.NewClient("http://localhost:9000", sentinelToken)
	if err != nil {
		t.Fatalf("expected no error for loopback http base (localhost), got: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil *Client for loopback http base")
	}
}

func TestNewClient_AcceptsHTTP_Loopback_IPv6(t *testing.T) {
	t.Parallel()
	c, err := techzone.NewClient("http://[::1]:9000", sentinelToken)
	if err != nil {
		t.Fatalf("expected no error for loopback http base (::1), got: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil *Client for loopback http base (::1)")
	}
}

func TestNewClient_RejectsNonHTTPS_NonLoopback(t *testing.T) {
	t.Parallel()
	c, err := techzone.NewClient("http://evil.com", sentinelToken)
	if err == nil {
		t.Fatal("expected error for non-https non-loopback base, got nil")
	}
	if c != nil {
		t.Fatal("expected nil *Client on validation error")
	}
	// Token safety: the error must not contain the sentinel.
	assertNoTokenLeak(t, err.Error())
}

func TestNewClient_RejectsNonHTTPS_NonLoopback_TokenSafe(t *testing.T) {
	t.Parallel()
	// Belt-and-suspenders: verify token safety is preserved even in error message.
	_, err := techzone.NewClient("http://evil.com", sentinelToken)
	if err == nil {
		t.Fatal("expected error for non-https non-loopback base")
	}
	assertNoTokenLeak(t, err.Error())
}

// ---------------------------------------------------------------------------
// NewClient — redirect policy
// ---------------------------------------------------------------------------

// TestNewClient_NoFollowRedirect verifies that the client's CheckRedirect
// policy returns http.ErrUseLastResponse, causing 3xx responses to be
// returned as-is rather than followed.
// This is the "status-wins" invariant: a 302 → SSO login surfaces as 302,
// not as the eventual SSO page's 200.
func TestNewClient_NoFollowRedirect_BehaviourIs302(t *testing.T) {
	t.Parallel()

	// httptest.Server that returns a 302 pointing to a second path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect-target" {
			// Should NEVER be reached — the client must not follow the redirect.
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<html>SSO login page</html>"))
			return
		}
		// First path: issue a 302.
		http.Redirect(w, r, "/redirect-target", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	c, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	status, _, doErr := c.DoGet(context.Background(), "/")
	if doErr != nil {
		t.Fatalf("DoGet returned unexpected error: %v", doErr)
	}
	if status != http.StatusFound {
		t.Errorf("expected status 302 (redirect not followed), got %d", status)
	}
}

// ---------------------------------------------------------------------------
// DoGet — token probe against httptest.Server
// ---------------------------------------------------------------------------

// TestDoGet_200_ValidJSON: 200 + JSON body → (200, body, nil). The caller
// treats this as a valid token; the test verifies that DoGet itself returns
// the status faithfully.
func TestDoGet_200_ValidJSON(t *testing.T) {
	t.Parallel()

	wantBody := `[{"id":"res-1","status":"Ready"}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify Authorization header presence (not value — we never record it).
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("expected Authorization: Bearer header, got none")
		}
		// Verify token does NOT appear anywhere in the URL.
		if strings.Contains(r.URL.RawQuery, sentinelToken) {
			t.Error("token safety violation: sentinel found in URL query params")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(wantBody))
	}))
	t.Cleanup(srv.Close)

	c, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	status, body, err := c.DoGet(context.Background(), "/api/my/reservations/all")
	if err != nil {
		t.Fatalf("DoGet returned unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("expected 200, got %d", status)
	}
	if !json.Valid(body) {
		t.Errorf("expected JSON-valid body, got: %s", body)
	}
}

// TestDoGet_200_HTMLBody: 200 + HTML body → DoGet returns (200, html, nil).
// The parseability check is the caller's responsibility (provider.Configure /
// ValidateConfig); this test confirms DoGet faithfully returns the raw body.
func TestDoGet_200_HTMLBody(t *testing.T) {
	t.Parallel()

	ssoHTML := `<html><body>Sign in to IBM</body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(ssoHTML))
	}))
	t.Cleanup(srv.Close)

	c, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	status, body, err := c.DoGet(context.Background(), "/api/my/reservations/all")
	if err != nil {
		t.Fatalf("DoGet returned unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("expected 200, got %d", status)
	}
	if json.Valid(body) {
		t.Errorf("expected non-JSON (HTML) body, but json.Valid returned true: %s", body)
	}
}

// TestDoGet_302_SSORedirect: 302 is NOT followed; DoGet returns status=302.
// This is the primary "expired token" signal (status-wins).
func TestDoGet_302_SSORedirect(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sso" {
			// Must NEVER be reached.
			t.Error("redirect was followed — CheckRedirect policy is broken")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
			return
		}
		http.Redirect(w, r, "/sso", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	c, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	status, _, err := c.DoGet(context.Background(), "/api/my/reservations/all")
	if err != nil {
		t.Fatalf("DoGet returned unexpected error on 302: %v", err)
	}
	if status != http.StatusFound {
		t.Errorf("expected status 302 (redirect not followed), got %d", status)
	}
}

// TestDoGet_401_Unauthorized: 401 returns (401, body, nil). Token safety checked.
func TestDoGet_401_Unauthorized(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Unauthorized"}`))
	}))
	t.Cleanup(srv.Close)

	c, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	status, _, err := c.DoGet(context.Background(), "/api/my/reservations/all")
	if err != nil {
		// DoGet must not treat 4xx as a Go error — status carries the signal.
		assertNoTokenLeak(t, err.Error())
		t.Fatalf("DoGet returned unexpected error on 401: %v", err)
	}
	if status != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", status)
	}
}

// TestDoGet_403_Forbidden: mirrors 401 test for 403.
func TestDoGet_403_Forbidden(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"Forbidden"}`))
	}))
	t.Cleanup(srv.Close)

	c, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	status, _, err := c.DoGet(context.Background(), "/api/my/reservations/all")
	if err != nil {
		assertNoTokenLeak(t, err.Error())
		t.Fatalf("DoGet returned unexpected error on 403: %v", err)
	}
	if status != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", status)
	}
}

// TestDoGet_TransportError: closed/unreachable server → connectivity-class error.
// The error must NOT contain the sentinel token (token safety).
func TestDoGet_TransportError_NoTokenLeak(t *testing.T) {
	t.Parallel()

	// Start a server, close it immediately — the port will be unreachable.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed before DoGet is called

	c, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	status, _, err := c.DoGet(context.Background(), "/api/my/reservations/all")
	if err == nil {
		t.Fatal("expected transport error for closed server, got nil")
	}
	if status != 0 {
		t.Errorf("expected status 0 on transport error, got %d", status)
	}
	// Token must not appear in any error string.
	assertNoTokenLeak(t, err.Error())
}

// TestDoGet_AuthorizationHeader_NeverInURL verifies that the sentinel token
// value never appears as a query parameter or URL fragment.
func TestDoGet_AuthorizationHeader_NeverInURL(t *testing.T) {
	t.Parallel()

	tokenFoundInURL := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check full raw URL (path + query).
		if strings.Contains(r.URL.String(), sentinelToken) {
			tokenFoundInURL = true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)

	c, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, _, _ = c.DoGet(context.Background(), "/api/my/reservations/all")

	if tokenFoundInURL {
		t.Error("token safety violation: sentinel token found in URL — must be header-only")
	}
}

// ---------------------------------------------------------------------------
// DoPost — Shin F-3 coverage (mirrors DoGet tests above)
// ---------------------------------------------------------------------------

// TestDoPost_CorrectMethodAndHeaders verifies that DoPost:
//   - uses HTTP POST (not GET/PUT/PATCH)
//   - sets Authorization: Bearer <api_key> header
//   - sets Content-Type: application/json
//   - never puts the token in the URL
func TestDoPost_CorrectMethodAndHeaders(t *testing.T) {
	t.Parallel()

	var gotMethod, gotAuth, gotContentType string
	tokenInURL := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		if strings.Contains(r.URL.String(), sentinelToken) {
			tokenInURL = true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"new-res"}`))
	}))
	t.Cleanup(srv.Close)

	c, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	status, body, err := c.DoPost(context.Background(), "/api/reservation/aws", []byte(`{"name":"test"}`))
	if err != nil {
		t.Fatalf("DoPost returned unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("expected status 200, got %d", status)
	}
	if !json.Valid(body) {
		t.Errorf("expected JSON body, got: %s", body)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected HTTP method POST, got %q", gotMethod)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("expected Authorization: Bearer header, got %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("expected Content-Type: application/json, got %q", gotContentType)
	}
	if tokenInURL {
		t.Error("token safety: sentinel found in URL — must be header-only")
	}
}

// TestDoPost_SendsBody verifies that the request body provided to DoPost is
// actually sent to the server.
func TestDoPost_SendsBody(t *testing.T) {
	t.Parallel()

	wantBody := `{"name":"test-reservation","region":"us-east-2"}`
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"res-123"}`))
	}))
	t.Cleanup(srv.Close)

	c, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, _, err = c.DoPost(context.Background(), "/api/reservation/aws", []byte(wantBody))
	if err != nil {
		t.Fatalf("DoPost: %v", err)
	}
	if string(gotBody) != wantBody {
		t.Errorf("DoPost sent body %q, want %q", gotBody, wantBody)
	}
}

// TestDoPost_Returns4xx verifies that DoPost returns the raw status code for
// 4xx/5xx responses without wrapping them as Go errors.
func TestDoPost_Returns4xx(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"invalid payload"}`))
	}))
	t.Cleanup(srv.Close)

	c, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	status, _, err := c.DoPost(context.Background(), "/api/reservation/aws", []byte(`{}`))
	if err != nil {
		// 4xx must NOT be a Go error — the status code carries the signal.
		assertNoTokenLeak(t, err.Error())
		t.Fatalf("DoPost returned unexpected Go error on 422: %v", err)
	}
	if status != http.StatusUnprocessableEntity {
		t.Errorf("expected status 422, got %d", status)
	}
}

// TestDoPost_TransportError_NoTokenLeak verifies that DoPost returns an error
// on transport failure and the sentinel token does NOT appear in the error string.
func TestDoPost_TransportError_NoTokenLeak(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed before DoPost is called

	c, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	status, _, err := c.DoPost(context.Background(), "/api/reservation/aws", []byte(`{}`))
	if err == nil {
		t.Fatal("expected transport error for closed server, got nil")
	}
	if status != 0 {
		t.Errorf("expected status 0 on transport error, got %d", status)
	}
	assertNoTokenLeak(t, err.Error())
}

// ---------------------------------------------------------------------------
// DoDelete — Shin F-3 coverage (mirrors DoGet / DoPost tests above)
// ---------------------------------------------------------------------------

// TestDoDelete_CorrectMethodAndHeaders verifies that DoDelete:
//   - uses HTTP DELETE
//   - sets Authorization: Bearer <api_key> header
//   - sets Content-Type: application/json
//   - never puts the token in the URL
func TestDoDelete_CorrectMethodAndHeaders(t *testing.T) {
	t.Parallel()

	var gotMethod, gotAuth, gotContentType string
	tokenInURL := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		if strings.Contains(r.URL.String(), sentinelToken) {
			tokenInURL = true
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	status, _, err := c.DoDelete(context.Background(), "/api/reservation/aws/test-id",
		[]byte(`{"IBMID":"user@example.com","requestType":"aws","reservationId":"test-id"}`))
	if err != nil {
		t.Fatalf("DoDelete returned unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("expected status 200, got %d", status)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("expected HTTP method DELETE, got %q", gotMethod)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("expected Authorization: Bearer header, got %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("expected Content-Type: application/json, got %q", gotContentType)
	}
	if tokenInURL {
		t.Error("token safety: sentinel found in URL — must be header-only")
	}
}

// TestDoDelete_SendsBody verifies that the JSON body provided to DoDelete is
// actually sent (the delete body contains IBMID/requestType/reservationId).
func TestDoDelete_SendsBody(t *testing.T) {
	t.Parallel()

	wantBody := `{"IBMID":"user@example.com","requestType":"aws","reservationId":"res-abc"}`
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, _, err = c.DoDelete(context.Background(), "/api/reservation/aws/res-abc", []byte(wantBody))
	if err != nil {
		t.Fatalf("DoDelete: %v", err)
	}
	if string(gotBody) != wantBody {
		t.Errorf("DoDelete sent body %q, want %q", gotBody, wantBody)
	}
}

// TestDoDelete_Returns204and404 verifies that DoDelete returns the raw status
// for idempotent-success codes 204 and 404 without wrapping them as Go errors.
func TestDoDelete_Returns204and404(t *testing.T) {
	t.Parallel()
	for _, wantCode := range []int{http.StatusNoContent, http.StatusNotFound} {
		wantCode := wantCode
		t.Run(fmt.Sprintf("HTTP_%d", wantCode), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(wantCode)
			}))
			t.Cleanup(srv.Close)

			c, err := techzone.NewClient(srv.URL, sentinelToken)
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			status, _, err := c.DoDelete(context.Background(), "/api/reservation/aws/test-id", nil)
			if err != nil {
				assertNoTokenLeak(t, err.Error())
				t.Fatalf("DoDelete returned unexpected Go error on %d: %v", wantCode, err)
			}
			if status != wantCode {
				t.Errorf("expected status %d, got %d", wantCode, status)
			}
		})
	}
}

// TestDoDelete_TransportError_NoTokenLeak_NoPIILeak verifies that DoDelete
// returns an error on transport failure and:
//   - the sentinel token does NOT appear in the error string (token safety)
//   - the PII sentinel (user_email placeholder) does NOT appear in the error (Ei F-07)
func TestDoDelete_TransportError_NoTokenLeak_NoPIILeak(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	c, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	const piiSentinel = "pii-user@example.com"
	deleteBody, _ := json.Marshal(map[string]string{
		"IBMID":         piiSentinel,
		"requestType":   "aws",
		"reservationId": "res-abc",
	})

	status, _, err := c.DoDelete(context.Background(), "/api/reservation/aws/res-abc", deleteBody)
	if err == nil {
		t.Fatal("expected transport error for closed server, got nil")
	}
	if status != 0 {
		t.Errorf("expected status 0 on transport error, got %d", status)
	}
	// Token safety.
	assertNoTokenLeak(t, err.Error())
	// PII safety (Ei F-07): the delete body contains user_email as IBMID —
	// the error string must not echo back the request body.
	if strings.Contains(err.Error(), piiSentinel) {
		t.Errorf("PII leak: user_email sentinel found in error: %q", err.Error())
	}
}
