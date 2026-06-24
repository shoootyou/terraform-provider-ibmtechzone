// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package techzone provides the HTTP client used by the terraform-provider-ibmtechzone
// provider to communicate with the IBM TechZone reservation API.
//
// # Token-safety contract (RFC §4 / Ei F-02)
//
// The api_key value is passed ONLY as an Authorization header value.
// It is NEVER formatted into logs, diagnostics, or error strings.
// No request/response dumping (httputil.DumpRequest et al.) is permitted.
// Error strings include only: HTTP status code, URL without query params,
// and a response-body excerpt — never request headers or the token value.
package techzone

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client holds the configuration for the TechZone API HTTP client.
// The embedded *http.Client must have its CheckRedirect set to return
// http.ErrUseLastResponse so that 3xx responses (SSO redirect) are
// observed raw and never followed.
type Client struct {
	apiBase    string
	apiKey     string
	httpClient *http.Client
}

// isLoopback reports whether host (as parsed from the URL) is a loopback address.
// Accepts exactly: "127.0.0.1", "localhost", "::1" (with or without brackets).
func isLoopback(host string) bool {
	switch host {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return false
}

// NewClient constructs a Client after validating apiBase.
//
// Rules (RFC §4, validate-token.sh loopback guard):
//   - https:// schemes are always accepted.
//   - http:// is accepted only when the host is a loopback address
//     (127.0.0.1, localhost, ::1) — enables httptest.Server in tests.
//   - Any other http:// (or unknown scheme) is rejected with a structured error.
//
// The resulting *http.Client:
//   - Sets CheckRedirect to return http.ErrUseLastResponse (never follows redirects).
//   - Has a 30-second overall timeout.
//
// A trailing slash on apiBase is normalized away so that path concatenation
// in DoGet/DoPost/DoDelete always produces a clean URL.
func NewClient(apiBase, apiKey string) (*Client, error) {
	parsed, err := url.Parse(apiBase)
	if err != nil {
		return nil, fmt.Errorf("invalid api_base URL: %w", err)
	}

	switch parsed.Scheme {
	case "https":
		// always accepted
	case "http":
		// accepted only for loopback hosts
		if !isLoopback(parsed.Hostname()) {
			return nil, fmt.Errorf(
				"api_base must use https:// (got %q): only loopback hosts (127.0.0.1, localhost, ::1) are permitted with http://",
				parsed.Scheme+"://"+parsed.Host,
			)
		}
	default:
		return nil, fmt.Errorf(
			"api_base must use https:// (got scheme %q)",
			parsed.Scheme,
		)
	}

	c := &Client{
		apiBase: strings.TrimRight(apiBase, "/"),
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				// Never follow redirects — 3xx surfaces as-is to caller.
				return http.ErrUseLastResponse
			},
		},
	}

	return c, nil
}

// DoGet performs a GET request to path (relative to c.apiBase).
// It sets the Authorization: Bearer header using c.apiKey and never logs
// or returns the token value. The returned status is the raw HTTP status
// code; body is the response body bytes.
func (c *Client) DoGet(ctx context.Context, path string) (status int, body []byte, err error) {
	reqURL := c.apiBase + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		// Do not include apiKey in the error.
		return 0, nil, fmt.Errorf("building request for %s: %w", path, err)
	}

	// Set Authorization header — never in URL.
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Transport failure — connectivity-class error. Do NOT include the token.
		return 0, nil, fmt.Errorf("connecting to TechZone API at %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("reading response body from %s: %w", path, err)
	}

	return resp.StatusCode, respBody, nil
}

// DoPost performs a POST request to path with the given body.
// Sets Authorization: Bearer and Content-Type: application/json headers.
// Returns (status, body, nil) on any completed HTTP response; (0, nil, err) on
// transport failure. The api_key is never included in error strings.
func (c *Client) DoPost(ctx context.Context, path string, body []byte) (status int, respBody []byte, err error) {
	reqURL := c.apiBase + path

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("building POST request for %s: %w", path, err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("connecting to TechZone API at %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("reading response body from %s: %w", path, err)
	}

	return resp.StatusCode, respBody, nil
}

// DoDelete performs a DELETE request to path with the given JSON body.
// Sets Authorization: Bearer and Content-Type: application/json headers.
// Returns (status, body, nil) on any completed HTTP response; (0, nil, err) on
// transport failure. The api_key is never included in error strings.
func (c *Client) DoDelete(ctx context.Context, path string, body []byte) (status int, respBody []byte, err error) {
	reqURL := c.apiBase + path

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("building DELETE request for %s: %w", path, err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("connecting to TechZone API at %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("reading response body from %s: %w", path, err)
	}

	return resp.StatusCode, respBody, nil
}
