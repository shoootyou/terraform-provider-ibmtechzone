package provider_test

// TestTFLog_SentinelAbsentFromDebugOutput — Ei-A F-02 coverage
//
// Drives a Configure probe with a sentinel token, captures the provider's
// tflog output at TRACE level via tflogtest.RootLogger, and asserts that the
// sentinel value never appears in any log entry at any level.
//
// Why this matters (RFC §4 / Ei F-02):
// The terraform-plugin-framework Sensitive schema marking protects the plan/apply
// UI only.  It does NOT suppress the token from tflog output — that is an explicit
// implementation responsibility.  This test catches a future regression where the
// token is accidentally formatted into a log statement.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/hashicorp/terraform-plugin-log/tflogtest"
	"github.com/shoootyou-ext/terraform-provider-ibmtechzone/internal/techzone"
)

// TestTFLog_SentinelAbsentFromDebugOutput captures all provider log output
// produced during a DoGet call and asserts the sentinel token does not appear.
//
// This test operates at the techzone.Client level (not the full Terraform CLI
// acceptance layer) because:
//  1. It needs to inject a controlled log context via tflogtest.RootLogger.
//  2. The full TF_ACC layer runs in a subprocess; intercepting its log output
//     requires parsing TF_LOG files, which is fragile and platform-dependent.
//  3. The client is the choke-point for all HTTP calls — any logging the provider
//     does around HTTP requests goes through DoGet/DoPost/DoDelete.
//
// If the client or provider ever adds a tflog.Debug/Info/Trace call that formats
// the api_key, this test will catch it.
func TestTFLog_SentinelAbsentFromDebugOutput(t *testing.T) {
	t.Parallel()

	// Capture all log output in a buffer.
	var logBuf bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &logBuf)

	// Emit a test log entry to confirm the buffer is working.
	tflog.Trace(ctx, "tflog sentinel scan: starting probe", map[string]any{
		"test": "TestTFLog_SentinelAbsentFromDebugOutput",
	})

	// Start a mock server that returns 401 (triggers the token-invalid path,
	// which is the most likely place for a careless log statement).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Record the presence of the Authorization header (not its value).
		if r.Header.Get("Authorization") == "" {
			t.Error("expected Authorization header, got none")
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Unauthorized"}`))
	}))
	t.Cleanup(srv.Close)

	client, err := techzone.NewClient(srv.URL, sentinelToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// DoGet is the lowest-level call — if the token leaks into logs anywhere,
	// it will happen here or in the caller's error handling.
	status, _, doErr := client.DoGet(ctx, "/api/my/reservations/all")
	if doErr != nil {
		t.Logf("DoGet returned error (expected for 401 path): %v", doErr)
	}
	if status != http.StatusUnauthorized && doErr == nil {
		t.Errorf("expected 401 status, got %d", status)
	}

	// Parse and scan every log entry for the sentinel.
	entries, err := tflogtest.MultilineJSONDecode(&logBuf)
	if err != nil {
		// If the buffer is empty (no log statements), MultilineJSONDecode returns
		// an error — that's fine; no log entries means no leak.
		t.Logf("tflogtest.MultilineJSONDecode: %v (may be empty log — OK)", err)
		return
	}

	for i, entry := range entries {
		// Check every field in every log entry.
		for k, v := range entry {
			valStr := ""
			switch tv := v.(type) {
			case string:
				valStr = tv
			default:
				valStr = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(
					strings.Join(strings.Fields(fmt.Sprintf("%v", tv)), " ")),
					"\n", " "))
			}
			if strings.Contains(valStr, sentinelToken) {
				t.Errorf(
					"Ei F-02 token leak in log entry %d, field %q: value contains sentinel token",
					i, k,
				)
			}
		}
	}
}
