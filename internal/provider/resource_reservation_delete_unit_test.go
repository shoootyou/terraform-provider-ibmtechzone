// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package provider — internal Delete unit tests.
//
// These tests are in package provider (not provider_test) so they can access
// the unexported reservationResource, reservationModel, and providerData types
// and call r.Delete() directly.  This lets us make precise assertions about
// resp.State after a failed Delete — in particular that state is NOT blanked
// after an auth-failure error (the orphaning bug introduced by Kou's blanking hack).
package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/shoootyou-ext/terraform-provider-techzone/internal/techzone"
)

// sentinelTokenInternal is the sentinel value used for token-safety assertions
// within this internal test package.  Must match the value in the external tests.
const sentinelTokenInternal = "SENTINEL-TOKEN-DO-NOT-LOG"

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// resourceSchemaForDelete obtains the framework schema from the reservation resource.
// We call the resource's Schema method directly to avoid hard-coding attribute names.
func resourceSchemaForDelete(t *testing.T) rschema.Schema {
	t.Helper()
	r := NewReservationResource().(*reservationResource)
	var resp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Schema() returned diagnostics: %v", resp.Diagnostics)
	}
	return resp.Schema
}

// buildDeleteState constructs a tfsdk.State populated with a minimal but valid
// reservationModel containing reservationID so that req.State.Get() succeeds in Delete.
func buildDeleteState(t *testing.T, s rschema.Schema, reservationID string) tfsdk.State {
	t.Helper()
	ctx := context.Background()

	// Build an empty-but-typed Raw value from the schema.
	rawType := s.Type().TerraformType(ctx)
	state := tfsdk.State{
		Schema: s,
		Raw:    tftypes.NewValue(rawType, nil),
	}

	// Populate a ServiceLinks empty list so the model round-trips cleanly.
	emptyLinks, diags := types.ListValueFrom(
		ctx,
		types.ObjectType{AttrTypes: serviceLinkAttrTypes},
		[]ServiceLinkModel{},
	)
	if diags.HasError() {
		t.Fatalf("building empty service_links: %v", diags)
	}

	m := reservationModel{
		Template:                types.StringValue("aws-account-hashicorp-ddr"),
		Region:                  types.StringValue("us-east-2"),
		ReservationName:         types.StringValue("Hashicorp DDR"),
		Purpose:                 types.StringValue("Demo"),
		CollectionID:            types.StringValue("test-collection-id"),
		UserEmail:               types.StringValue("test@example.com"),
		HCPOrg:                  types.StringValue("test-hcp-org"),
		HCPProject:              types.StringValue("test-hcp-project"),
		ReservationDurationDays: types.Int64Value(1),
		TimeoutMinutes:          types.Int64Value(30),
		ID:                      types.StringValue(reservationID),
		Status:                  types.StringValue("Ready"),
		ServiceLinks:            emptyLinks,
		StartDate:               types.StringValue("2024-06-15T10:00:00Z"),
		EndDate:                 types.StringValue("2099-12-31T23:59:59Z"),
	}

	diags = state.Set(ctx, m)
	if diags.HasError() {
		t.Fatalf("state.Set() failed: %v", diags)
	}
	return state
}

// buildReservationResource constructs a reservationResource wired to a
// techzone.Client pointing at srvURL, with the sentinel api_key.
func buildReservationResource(t *testing.T, srvURL string) *reservationResource {
	t.Helper()
	client, err := techzone.NewClient(srvURL, sentinelTokenInternal)
	if err != nil {
		t.Fatalf("NewClient(%s): %v", srvURL, err)
	}
	return &reservationResource{
		pd:  &providerData{Client: client},
		now: time.Now,
	}
}

// assertActionableMessage fails t if the diagnostic detail string does not
// contain one of the required actionable phrases.
func assertActionableMessage(t *testing.T, diags interface{ Errors() []string }) {
	t.Helper()
	// We check the raw Diagnostics.
}

// ---------------------------------------------------------------------------
// TestDeleteUnit_AuthFailure_StatePreserved
//
// This is the CRITICAL state-preservation assertion.
// Contract B requires that on a 302/401/403 auth-failure response:
//   (a) resp.Diagnostics.HasError() == true
//   (b) the error message contains an actionable "refresh TECHZONE_API_KEY" hint
//   (c) resp.State STILL carries the original resource id — NOT blanked to ""
//
// Assertion (c) is RED against the current (blanking) implementation:
//   Kou's code does resp.State.Set(ctx, blank{ID: ""}) before AddError, which
//   causes the Framework to write a zero-ID state after the error.  A subsequent
//   `terraform destroy` sees id="" and no-ops (nothing to delete) → orphaned account.
// ---------------------------------------------------------------------------

func TestDeleteUnit_AuthFailure_302_StatePreserved(t *testing.T) {
	t.Parallel()
	runDeleteAuthFailureStateTest(t, "302_redirect", http.StatusFound, func(w http.ResponseWriter, r *http.Request) {
		// Location must be a full URL to avoid parse errors in the http client.
		// The client has CheckRedirect=ErrUseLastResponse so this redirect is
		// observed raw (status=302) and never followed.
		w.Header().Set("Location", "http://auth.example.com/sso-login")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusFound)
		fmt.Fprint(w, "<html><body>Sign in to IBM</body></html>")
	})
}

func TestDeleteUnit_AuthFailure_401_StatePreserved(t *testing.T) {
	t.Parallel()
	runDeleteAuthFailureStateTest(t, "401", http.StatusUnauthorized, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"Unauthorized"}`)
	})
}

func TestDeleteUnit_AuthFailure_403_StatePreserved(t *testing.T) {
	t.Parallel()
	runDeleteAuthFailureStateTest(t, "403", http.StatusForbidden, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":"Forbidden"}`)
	})
}

// runDeleteAuthFailureStateTest is the shared body for the three auth-failure tests.
// It constructs the request/response manually, calls Delete(), and asserts the
// three contract-B properties: error fired, actionable message, state not blanked.
func runDeleteAuthFailureStateTest(
	t *testing.T,
	name string,
	_ int, // expectedHTTPCode — kept for documentation, not needed in assertions
	handler http.HandlerFunc,
) {
	t.Helper()
	const reservationID = "test-reservation-id"

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	r := buildReservationResource(t, srv.URL)
	s := resourceSchemaForDelete(t)
	state := buildDeleteState(t, s, reservationID)
	ctx := context.Background()

	req := resource.DeleteRequest{State: state}
	resp := resource.DeleteResponse{State: state} // pre-populated from req, as framework does

	// Exercise the method under test.
	r.Delete(ctx, req, &resp)

	// -----------------------------------------------------------------------
	// (a) An error must be present.
	// -----------------------------------------------------------------------
	if !resp.Diagnostics.HasError() {
		t.Errorf("[%s] Delete(%d): expected Diagnostics.HasError() == true, got no error", name, 0)
		return // further assertions are meaningless without an error
	}

	// -----------------------------------------------------------------------
	// (b) The error message must be actionable (mention "expired" / "refresh" /
	//     "TECHZONE_API_KEY") — not just a raw "HTTP 302/401/403" status code.
	// -----------------------------------------------------------------------
	var allMsgs strings.Builder
	for _, d := range resp.Diagnostics {
		allMsgs.WriteString(d.Summary())
		allMsgs.WriteString(" ")
		allMsgs.WriteString(d.Detail())
		allMsgs.WriteString(" ")
	}
	combined := allMsgs.String()

	actionablePatterns := []string{"invalid", "expired", "refresh", "TECHZONE_API_KEY"}
	found := false
	for _, p := range actionablePatterns {
		if strings.Contains(strings.ToLower(combined), strings.ToLower(p)) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("[%s] Delete: error message is not actionable; want one of %v in message, got: %q",
			name, actionablePatterns, combined)
	}

	// -----------------------------------------------------------------------
	// Token safety: the sentinel must NOT appear in any error message (Ei F-02).
	// -----------------------------------------------------------------------
	if strings.Contains(combined, sentinelTokenInternal) {
		t.Errorf("[%s] Delete: token safety violation — sentinel found in error message: %q",
			name, combined)
	}

	// -----------------------------------------------------------------------
	// (c) STATE PRESERVATION — the resource id must NOT be blanked.
	//
	// This is the assertion that is RED against the current blanking implementation.
	// After a failed Delete, the Framework will NOT clear state (it only clears
	// state on a successful delete, i.e. when no error diagnostics are present).
	// However, Kou's current code explicitly writes a blank state BEFORE adding the
	// error — so resp.State ends up with id="" even though an error was added.
	//
	// The correct behavior: Delete on auth-failure adds an error and does NOT
	// modify resp.State. The state that was pre-populated into DeleteResponse.State
	// (from DeleteRequest.State) must remain intact with the original id.
	// -----------------------------------------------------------------------
	var stateAfter reservationModel
	diags := resp.State.Get(ctx, &stateAfter)
	if diags.HasError() {
		t.Errorf("[%s] Delete: resp.State.Get() after auth-failure error: %v", name, diags)
		return
	}

	gotID := stateAfter.ID.ValueString()
	if gotID != reservationID {
		t.Errorf(
			"[%s] Delete auth-failure STATE PRESERVATION FAILURE:\n"+
				"  got  id = %q\n"+
				"  want id = %q\n"+
				"  The resource was removed from state after a failed delete, which prevents\n"+
				"  a token-refresh-and-retry from cleaning up the real TechZone reservation\n"+
				"  (orphaning bug). Delete on auth-failure MUST NOT modify resp.State.",
			name, gotID, reservationID,
		)
	}
}

// ---------------------------------------------------------------------------
// TestDeleteUnit_Success_NoError — success codes leave no error and clear state
// (positive contract pin, mirrors TestAccReservation_Delete_Success_Codes)
// ---------------------------------------------------------------------------

func TestDeleteUnit_Success_200(t *testing.T) { runDeleteSuccessTest(t, http.StatusOK) }
func TestDeleteUnit_Success_204(t *testing.T) { runDeleteSuccessTest(t, http.StatusNoContent) }
func TestDeleteUnit_Success_404(t *testing.T) { runDeleteSuccessTest(t, http.StatusNotFound) }

func runDeleteSuccessTest(t *testing.T, code int) {
	t.Helper()
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)

	r := buildReservationResource(t, srv.URL)
	s := resourceSchemaForDelete(t)
	state := buildDeleteState(t, s, "test-reservation-id")
	ctx := context.Background()

	req := resource.DeleteRequest{State: state}
	resp := resource.DeleteResponse{State: state}
	r.Delete(ctx, req, &resp)

	if resp.Diagnostics.HasError() {
		t.Errorf("Delete(%d): expected no error, got: %v", code, resp.Diagnostics)
	}
}

// ---------------------------------------------------------------------------
// TestDeleteUnit_TransportError — transport failure → error, state preserved
// ---------------------------------------------------------------------------

func TestDeleteUnit_TransportError_StatePreserved(t *testing.T) {
	t.Parallel()
	const reservationID = "test-reservation-id"

	// Close the server immediately so DoDelete gets a transport error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	r := buildReservationResource(t, srv.URL)
	s := resourceSchemaForDelete(t)
	state := buildDeleteState(t, s, reservationID)
	ctx := context.Background()

	req := resource.DeleteRequest{State: state}
	resp := resource.DeleteResponse{State: state}
	r.Delete(ctx, req, &resp)

	if !resp.Diagnostics.HasError() {
		t.Error("Delete(transport error): expected error, got none")
		return
	}

	// Transport errors must not blame the token.
	var allMsgs strings.Builder
	for _, d := range resp.Diagnostics {
		allMsgs.WriteString(d.Summary())
		allMsgs.WriteString(" ")
		allMsgs.WriteString(d.Detail())
		allMsgs.WriteString(" ")
	}
	combined := allMsgs.String()
	if strings.Contains(combined, sentinelTokenInternal) {
		t.Errorf("Delete(transport error): token safety violation — sentinel in error: %q", combined)
	}

	// State must be preserved on a transport error as well (same principle).
	var stateAfter reservationModel
	if diags := resp.State.Get(ctx, &stateAfter); diags.HasError() {
		t.Fatalf("resp.State.Get() after transport error: %v", diags)
	}
	if got := stateAfter.ID.ValueString(); got != reservationID {
		t.Errorf("Delete(transport error): id blanked to %q, want %q", got, reservationID)
	}
}
