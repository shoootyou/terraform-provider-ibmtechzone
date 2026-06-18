// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package techzone provides the HTTP client used by the terraform-provider-techzone
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
	"context"
	"errors"
	"net/http"
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
// opts is reserved for future functional options (e.g. custom *http.Client
// injection in tests); callers pass nothing today.
func NewClient(apiBase, apiKey string, opts ...func(*Client)) (*Client, error) {
	// TODO(kou): implement — validate scheme, build *http.Client, apply opts.
	return nil, errors.New("not implemented")
}

// DoGet performs a GET request to path (relative to c.apiBase).
// It sets the Authorization: Bearer header using c.apiKey and never logs
// or returns the token value. The returned status is the raw HTTP status
// code; body is the response body bytes.
func (c *Client) DoGet(ctx context.Context, path string) (status int, body []byte, err error) {
	// TODO(kou): implement.
	return 0, nil, errors.New("not implemented")
}
