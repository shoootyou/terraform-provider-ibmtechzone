// Package provider — internal (white-box) unit tests for Create()'s poll
// loop and Final GET call sites (r2 finding 1 — see
// resource_reservation_extension_test.go's "r2 finding 1" section for the
// analogous Read()-initial-GET test and the shared background: the 401/403/302
// body_preview redaction guard fixed in round 1 for the extension-POST's
// succeeded/ambiguous branches was not retrofitted to these two sibling call
// sites, which share the same risk pattern and endpoint base).
//
// These tests call r.Create() directly (same convention as
// resource_reservation_delete_unit_test.go / resource_reservation_extension_test.go)
// rather than going through the !unit acceptance harness, because capturing
// tflog output requires an in-process call — the acceptance harness runs the
// provider via a reattached subprocess, and intercepting its log output would
// require parsing TF_LOG files (fragile, platform-dependent — see
// tflog_sentinel_test.go's own rationale for the same design choice).
//
// Timing note: the poll loop and Final GET both wait on a real 10-second
// ticker between retries. To exercise their non-2xx log branch without
// waiting 10 real seconds, these tests bound the request context with a short
// timeout (createRedactionCtxTimeout): the branch under test always fires on
// the FIRST GET call (no wait involved — the poll loop's attempt-0 fires
// immediately, and the Final GET loop always GETs before waiting), and the
// short ctx timeout then wins the subsequent `select { ctx.Done() /
// ticker.C }` race by four orders of magnitude, causing Create() to return
// promptly via "Context cancelled" instead of blocking for 10s.
package provider

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-log/tflogtest"
	"github.com/shoootyou-ext/terraform-provider-ibmtechzone/internal/techzone"
)

// createRedactionCtxTimeout bounds each test's Create() call well under the
// poll loop/Final GET's real 10-second ticker (see file-level comment).
const createRedactionCtxTimeout = 1 * time.Second

// createRedactionSentinelBody is embedded in a fabricated 302 response body
// for the poll-loop/Final-GET redaction tests below.
const createRedactionSentinelBody = "SENTINEL-CREATE-POLL-BODY-DO-NOT-LOG"

// createRedactionCollectionID / createRedactionCollectionJSON: a minimal
// valid DDR-shaped AWS collection response. Same shape as ddrCollectionJSON
// in resource_reservation_create_wired_test.go / testutil_mock_server_test.go
// (both package provider_test), reproduced here because this file is package
// provider (internal, needs unexported reservationResource access) and
// cannot import symbols from a sibling _test-only external package.
const createRedactionCollectionID = "69650af0758b9e41de66b6ae"

const createRedactionCollectionJSON = `{
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

// pollFixtureResp is one canned (status, body) response served by
// createPollFixture's GET /api/reservation/aws/<id> handler.
type pollFixtureResp struct {
	status int
	body   string
}

// createPollFixture is a minimal in-process HTTP fixture exercising
// reservationResource.Create()'s full call sequence — GET collection, POST
// create, then a shared, call-count-sequenced GET serving BOTH the poll loop
// and the Final GET (both hit the identical /api/reservation/aws/<id> path;
// which logical loop is "under test" is determined entirely by the
// sequence's contents, not by the URL).
type createPollFixture struct {
	t  *testing.T
	mu sync.Mutex

	// getSequence[n] is served for the nth GET /api/reservation/aws/<id>
	// call (0-indexed); once exhausted, the last element repeats.
	getSequence  []pollFixtureResp
	getCallCount int
}

func newCreatePollFixture(t *testing.T, getSequence []pollFixtureResp) *createPollFixture {
	t.Helper()
	if len(getSequence) == 0 {
		t.Fatal("newCreatePollFixture: getSequence must have at least one element")
	}
	return &createPollFixture{t: t, getSequence: getSequence}
}

// Server starts the httptest.Server and registers cleanup.
func (f *createPollFixture) Server() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	f.t.Cleanup(srv.Close)
	return srv
}

// GetCallCount returns how many times GET /api/reservation/aws/<id> has been
// called (poll loop + Final GET combined — the two tests below are each
// designed so only one of the two is ever actually exercised per run; see
// their individual comments).
func (f *createPollFixture) GetCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCallCount
}

func (f *createPollFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/collection/"):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, createRedactionCollectionJSON)

	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/reservation/aws"):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"create-redaction-res-1"}`)

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/reservation/aws/"):
		f.mu.Lock()
		idx := f.getCallCount
		f.getCallCount++
		resp := f.getSequence[len(f.getSequence)-1]
		if idx < len(f.getSequence) {
			resp = f.getSequence[idx]
		}
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		fmt.Fprint(w, resp.body)

	default:
		f.t.Logf("createPollFixture: unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

// buildCreateTestResource mirrors buildExtensionTestResource /
// buildReservationResource (same file-convention across this package).
func buildCreateTestResource(t *testing.T, srvURL string) *reservationResource {
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

// buildCreateTestPlan constructs a tfsdk.Plan populated with a minimal, valid
// reservationModel (identity fields required by Create() before it ever
// touches the poll loop / Final GET: collection_id, user_email,
// template_variables, requester_context; operational fields
// reservation_duration_days/timeout_minutes/extension_window_fraction).
//
// tfsdk.Plan — unlike tfsdk.State — has no public Set() method. Built via a
// throwaway tfsdk.State (which does have Set()) and repackaged: Plan and
// State share the identical underlying {Raw tftypes.Value, Schema
// fwschema.Schema} shape, so this is a pure data-construction convenience,
// not a semantic conflation of "plan" and "state".
func buildCreateTestPlan(t *testing.T, s rschema.Schema, timeoutMinutes int64) tfsdk.Plan {
	t.Helper()
	ctx := context.Background()

	rawType := s.Type().TerraformType(ctx)
	tmp := tfsdk.State{Schema: s, Raw: tftypes.NewValue(rawType, nil)}

	emptyLinks, diags := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: serviceLinkAttrTypes}, []ServiceLinkModel{})
	if diags.HasError() {
		t.Fatalf("building empty service_links: %v", diags)
	}

	dynMap, dynDiags := types.MapValue(types.StringType, map[string]attr.Value{})
	if dynDiags.HasError() {
		t.Fatalf("building empty template_variables map: %v", dynDiags)
	}

	rcAttrTypes := map[string]attr.Type{
		"opportunity": types.ListType{ElemType: types.StringType},
		"iui":         types.StringType,
	}
	nullRC := types.ObjectNull(rcAttrTypes)

	m := reservationModel{
		TemplateVariables:       dynMap,
		Region:                  types.StringValue("us-east-2"),
		ReservationName:         types.StringValue("Reservation Name"),
		Purpose:                 types.StringValue("Demo"),
		CollectionID:            types.StringValue(createRedactionCollectionID),
		UserEmail:               types.StringValue("test@example.com"),
		RequesterContext:        nullRC,
		ReservationDurationDays: types.Int64Value(4),
		TimeoutMinutes:          types.Int64Value(timeoutMinutes),
		ExtensionWindowFraction: types.Float64Value(0.5),
		ID:                      types.StringValue(""),
		Status:                  types.StringValue(""),
		ServiceLinks:            emptyLinks,
		StartDate:               types.StringValue(""),
		EndDate:                 types.StringValue(""),
		ExtendCount:             types.Int64Value(0),
	}

	if diags := tmp.Set(ctx, m); diags.HasError() {
		t.Fatalf("plan construction via throwaway State.Set() failed: %v", diags)
	}
	return tfsdk.Plan{Schema: s, Raw: tmp.Raw}
}

// captureAllTFLogEntriesWithContext mirrors resource_reservation_extension_test.go's
// captureAllTFLogEntries, but accepts an explicit parent context so a
// deadline/cancellation can be layered underneath the tflog wiring —
// necessary here to bound Create()'s real 10-second poll ticker (see
// file-level comment). tflogtest.RootLogger wraps ctx additively (adds a
// logger handle to the context), which preserves Done()/Err()/Deadline()
// delegation to the parent context — confirmed empirically below (the
// "Context cancelled" diagnostic firing on schedule IS this delegation
// working correctly).
func captureAllTFLogEntriesWithContext(t *testing.T, parentCtx context.Context, fn func(ctx context.Context)) []map[string]interface{} {
	t.Helper()
	var buf bytes.Buffer
	ctx := tflogtest.RootLogger(parentCtx, &buf)

	fn(ctx)

	entries, err := tflogtest.MultilineJSONDecode(&buf)
	if err != nil {
		t.Logf("captureAllTFLogEntriesWithContext: MultilineJSONDecode: %v (may be empty log)", err)
		return nil
	}
	return entries
}

// assertNoSentinelLeak scans every field of every log entry for sentinel,
// mirroring resource_reservation_extension_test.go's established
// sentinel-scan convention (runExtensionAuthOrRedirectBodyRedactedTest).
func assertNoSentinelLeak(t *testing.T, entries []map[string]interface{}, sentinel, scope string) {
	t.Helper()
	for i, e := range entries {
		for k, v := range e {
			if strings.Contains(fmt.Sprintf("%v", v), sentinel) {
				t.Errorf("r2 finding 1 (%s): log entry %d field %q leaked the response body sentinel: %v",
					scope, i, k, v)
			}
		}
	}
}

// assertContextCancelledDiagnostic confirms Create() bailed out via the
// expected "Context cancelled" mechanism (createRedactionCtxTimeout expiring)
// rather than some other, unintended error path — a sanity check that these
// tests are exercising the branch they claim to.
func assertContextCancelledDiagnostic(t *testing.T, resp *resource.CreateResponse) {
	t.Helper()
	if !resp.Diagnostics.HasError() {
		t.Fatal("Create(): expected the bounded context timeout to trigger \"Context cancelled\", got no error at all")
	}
	found := false
	for _, d := range resp.Diagnostics {
		if strings.Contains(d.Summary(), "Context cancelled") {
			found = true
			break
		}
	}
	if !found {
		var allMsgs strings.Builder
		for _, d := range resp.Diagnostics {
			allMsgs.WriteString(d.Summary())
			allMsgs.WriteString(": ")
			allMsgs.WriteString(d.Detail())
			allMsgs.WriteString(" | ")
		}
		t.Fatalf("Create(): expected a \"Context cancelled\" diagnostic (confirming the intended "+
			"branch was exercised), got a different error instead: %s", allMsgs.String())
	}
}

// ---------------------------------------------------------------------------
// r3 audit finding 1 (round 3, CRITICAL) — Create()'s own initial POST
// /api/reservation/aws (the create call itself, BEFORE GetCollection()'s
// result is ever used to poll) had zero 401/403/302 guard: any auth-failure
// or SSO-redirect response leaked up to 512 raw bytes directly into
// resp.Diagnostics. This is the 4th and final sibling of the same guard
// family (poll loop / Final GET above, Read()'s own initial GET in
// resource_reservation_extension_test.go).
// ---------------------------------------------------------------------------

// createPostFixture is a minimal in-process HTTP fixture exercising only
// reservationResource.Create()'s GetCollection() + create-POST call
// sequence — the poll loop and Final GET are never reached because the
// create-POST's own auth-failure guard returns before either is dialed.
type createPostFixture struct {
	t  *testing.T
	mu sync.Mutex

	postStatus    int
	postBody      string
	postCallCount int
}

func newCreatePostFixture(t *testing.T, postStatus int, postBody string) *createPostFixture {
	t.Helper()
	return &createPostFixture{t: t, postStatus: postStatus, postBody: postBody}
}

// Server starts the httptest.Server and registers cleanup.
func (f *createPostFixture) Server() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	f.t.Cleanup(srv.Close)
	return srv
}

// PostCallCount returns how many times POST /api/reservation/aws has been called.
func (f *createPostFixture) PostCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.postCallCount
}

func (f *createPostFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/collection/"):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, createRedactionCollectionJSON)

	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/reservation/aws"):
		f.mu.Lock()
		f.postCallCount++
		status, body := f.postStatus, f.postBody
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)

	default:
		f.t.Logf("createPostFixture: unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

// runCreatePOSTAuthOrRedirectBodyRedactedTest exercises Create()'s own
// initial POST /api/reservation/aws with the given auth-failure/redirect
// status and confirms the sentinel-embedded body never leaks into
// resp.Diagnostics — mirrors
// resource_reservation_extension_test.go's runExtensionAuthOrRedirectBodyRedactedTest
// convention, applied to the create-POST call site instead of the
// extension-POST.
func runCreatePOSTAuthOrRedirectBodyRedactedTest(t *testing.T, postStatus int) {
	t.Helper()
	sentinelBody := fmt.Sprintf(`<html><body>Sign in to IBM — session %s</body></html>`, createRedactionSentinelBody)
	fx := newCreatePostFixture(t, postStatus, sentinelBody)
	srv := fx.Server()

	r := buildCreateTestResource(t, srv.URL)
	s := resourceSchemaForDelete(t)
	plan := buildCreateTestPlan(t, s, 1)

	req := resource.CreateRequest{Plan: plan}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: plan.Raw}}

	// No bounded context timeout needed here (unlike the poll-loop/Final-GET
	// tests above): the create-POST's own auth-failure guard returns
	// immediately, before the poll loop's 10-second ticker is ever created.
	entries := captureAllTFLogEntriesWithContext(t, context.Background(), func(ctx context.Context) {
		r.Create(ctx, req, &resp)
	})

	if !resp.Diagnostics.HasError() {
		t.Fatalf("Create(): a %d on the create-POST must raise an auth-failure error, got none", postStatus)
	}

	var allMsgs strings.Builder
	for _, d := range resp.Diagnostics {
		allMsgs.WriteString(d.Summary())
		allMsgs.WriteString(" ")
		allMsgs.WriteString(d.Detail())
		allMsgs.WriteString(" ")
	}
	if combined := allMsgs.String(); strings.Contains(combined, createRedactionSentinelBody) {
		t.Errorf("r3 audit finding 1: Create()'s own POST /api/reservation/aws (status %d) leaked "+
			"the response body sentinel into Diagnostics — the 401/403/302 guard must route to the "+
			"canned auth-failure message, never the generic non-2xx branch that embeds the raw body: %q",
			postStatus, combined)
	}

	assertNoSentinelLeak(t, entries, createRedactionSentinelBody, fmt.Sprintf("create-POST %d", postStatus))

	if got := fx.PostCallCount(); got != 1 {
		t.Errorf("Create(): POST /api/reservation/aws called %d times, want exactly 1", got)
	}
}

// TestCreateUnit_InitialPOST401_BodyNotLeakedToDiagnostics: a 401 on
// Create()'s own create-POST must not leak its body.
func TestCreateUnit_InitialPOST401_BodyNotLeakedToDiagnostics(t *testing.T) {
	t.Parallel()
	runCreatePOSTAuthOrRedirectBodyRedactedTest(t, http.StatusUnauthorized)
}

// TestCreateUnit_InitialPOST403_BodyNotLeakedToDiagnostics: a 403 on
// Create()'s own create-POST must not leak its body.
func TestCreateUnit_InitialPOST403_BodyNotLeakedToDiagnostics(t *testing.T) {
	t.Parallel()
	runCreatePOSTAuthOrRedirectBodyRedactedTest(t, http.StatusForbidden)
}

// TestCreateUnit_InitialPOST302_BodyNotLeakedToDiagnostics: the client never
// follows redirects (CheckRedirect returns http.ErrUseLastResponse), so a raw
// 302 can reach Create()'s own create-POST directly and may carry SSO
// redirect HTML or session metadata; must not leak its body.
func TestCreateUnit_InitialPOST302_BodyNotLeakedToDiagnostics(t *testing.T) {
	t.Parallel()
	runCreatePOSTAuthOrRedirectBodyRedactedTest(t, http.StatusFound)
}

// ---------------------------------------------------------------------------
// r2 finding 1 (audit round 2, HIGH), poll loop — see the file-level comment
// and resource_reservation_extension_test.go's "r2 finding 1" section for
// full background.
// ---------------------------------------------------------------------------

// TestCreateUnit_PollLoop302_BodyPreviewRedacted: the poll loop's non-2xx
// branch (resource_reservation.go, Create()) must suppress body_preview for
// 401/403/302, same as the extension-POST branches fixed in round 1. Every
// GET /api/reservation/aws/<id> call returns 302 with a sentinel body, so the
// poll loop's very first attempt (attempt=0, which fires immediately, no
// wait) exercises the guarded log line; the bounded context timeout then
// stops the loop before any real 10-second ticker wait.
func TestCreateUnit_PollLoop302_BodyPreviewRedacted(t *testing.T) {
	t.Parallel()

	sentinelBody := fmt.Sprintf(`<html><body>Sign in to IBM — session %s</body></html>`, createRedactionSentinelBody)
	fx := newCreatePollFixture(t, []pollFixtureResp{
		{status: http.StatusFound, body: sentinelBody}, // every GET call (sequence has 1 element) → 302+sentinel
	})
	srv := fx.Server()

	r := buildCreateTestResource(t, srv.URL)
	s := resourceSchemaForDelete(t)
	plan := buildCreateTestPlan(t, s, 1) // timeoutMinutes=1 → maxAttempts=6 (irrelevant here — ctx bails out first)

	req := resource.CreateRequest{Plan: plan}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: plan.Raw}}

	ctx, cancel := context.WithTimeout(context.Background(), createRedactionCtxTimeout)
	defer cancel()

	entries := captureAllTFLogEntriesWithContext(t, ctx, func(ctx context.Context) {
		r.Create(ctx, req, &resp)
	})

	assertContextCancelledDiagnostic(t, &resp)

	if got := fx.GetCallCount(); got != 1 {
		t.Errorf("Create(): GET /api/reservation/aws/<id> called %d times, want exactly 1 "+
			"(the poll loop's attempt=0 fires immediately; attempt=1's pre-wait select must resolve "+
			"via ctx.Done() before ever retrying) — a count > 1 means this test drifted into "+
			"exercising the Final GET path instead of the poll loop", got)
	}

	assertNoSentinelLeak(t, entries, createRedactionSentinelBody, "poll loop")
}

// ---------------------------------------------------------------------------
// r2 finding 1 (audit round 2, HIGH), Final GET — see the file-level comment
// and resource_reservation_extension_test.go's "r2 finding 1" section for
// full background.
// ---------------------------------------------------------------------------

// TestCreateUnit_FinalGET302_BodyPreviewRedacted: the Final GET's non-2xx
// branch (resource_reservation.go, Create(), the `pollDone:` block) must
// suppress body_preview for 401/403/302, same as the poll loop above. The
// GET sequence serves 200+Ready on the FIRST call (poll loop succeeds,
// transitions to Final GET) and 302+sentinel on the second call (Final GET's
// own attempt=0) — isolating the Final GET's guarded log line specifically,
// distinct from the poll loop test above.
func TestCreateUnit_FinalGET302_BodyPreviewRedacted(t *testing.T) {
	t.Parallel()

	sentinelBody := fmt.Sprintf(`<html><body>Sign in to IBM — session %s</body></html>`, createRedactionSentinelBody)
	fx := newCreatePollFixture(t, []pollFixtureResp{
		{status: http.StatusOK, body: `{"status":"Ready"}`}, // poll loop attempt=0 → Ready → goto pollDone
		{status: http.StatusFound, body: sentinelBody},      // Final GET attempt=0 → 302+sentinel
	})
	srv := fx.Server()

	r := buildCreateTestResource(t, srv.URL)
	s := resourceSchemaForDelete(t)
	plan := buildCreateTestPlan(t, s, 1)

	req := resource.CreateRequest{Plan: plan}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: plan.Raw}}

	ctx, cancel := context.WithTimeout(context.Background(), createRedactionCtxTimeout)
	defer cancel()

	entries := captureAllTFLogEntriesWithContext(t, ctx, func(ctx context.Context) {
		r.Create(ctx, req, &resp)
	})

	assertContextCancelledDiagnostic(t, &resp)

	if got := fx.GetCallCount(); got != 2 {
		t.Errorf("Create(): GET /api/reservation/aws/<id> called %d times, want exactly 2 "+
			"(1 poll-loop attempt that succeeds with Ready, then 1 Final GET attempt that gets "+
			"302+sentinel) — a different count means this test did not reach the Final GET branch "+
			"as intended", got)
	}

	assertNoSentinelLeak(t, entries, createRedactionSentinelBody, "Final GET")
}
