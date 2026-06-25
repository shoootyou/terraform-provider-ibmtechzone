package provider_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// ---------------------------------------------------------------------------
// ibmtechzone_token_validation data source tests
// ---------------------------------------------------------------------------

// TestDataSource_TokenValidation_ReturnsStatusValid verifies that with a correctly
// configured provider (mock server returning 200 + JSON), the
// ibmtechzone_token_validation data source:
//   - reads successfully with no diagnostics
//   - exposes status = "valid"
//
// This is the primary RED test: the stub Read() sets status = "" so the
// assertion will fail until Kou implements the real Read().
func TestDataSource_TokenValidation_ReturnsStatusValid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"id":"res-1","status":"Ready"}]`))
	}))
	t.Cleanup(srv.Close)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactoriesFor(srv.URL, sentinelToken),
		Steps: []resource.TestStep{
			{
				Config: providerConfigHCL(srv.URL, sentinelToken) + `
data "ibmtechzone_token_validation" "test" {}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					// status must be exactly "valid" — the stub sets "" so this FAILS (RED).
					resource.TestCheckResourceAttr(
						"data.ibmtechzone_token_validation.test",
						"status",
						"valid",
					),
				),
			},
		},
	})
}

// TestDataSource_TokenValidation_ZeroInputs verifies that the data source schema
// accepts a zero-input config block (no attributes set by the user).
// This should compile and pass even before implementation, since the schema
// has no required inputs and no unknown attributes.
func TestDataSource_TokenValidation_ZeroInputs(t *testing.T) {
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
provider "ibmtechzone" {}
data "ibmtechzone_token_validation" "test" {}
`,
				// This will fail at Configure (no real token), but the data source
				// schema itself should not produce unknown-attribute errors.
				// We expect a Configure-level error, not a schema error.
			},
		},
	})
}

// TestDataSource_TokenValidation_RegistrationInProvider verifies that the provider
// actually registers the ibmtechzone_token_validation data source type in DataSources().
// An unregistered data source would produce "Invalid data source type" at plan.
func TestDataSource_TokenValidation_RegistrationInProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: providerFactoriesFor(srv.URL, sentinelToken),
		Steps: []resource.TestStep{
			{
				Config: providerConfigHCL(srv.URL, sentinelToken) + `
data "ibmtechzone_token_validation" "probe" {}
`,
				// The data source must be recognised (no "Invalid data source type" error).
				// The status value check is in TestDataSource_TokenValidation_ReturnsStatusValid.
			},
		},
	})
}
