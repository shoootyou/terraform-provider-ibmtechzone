# techzone_token_validation (Data Source)

Acts as a plan-time gate. Reading this data source validates the configured
`api_key` against the TechZone API. If the token is expired or invalid, the
plan fails immediately — before any `techzone_reservation` resource is evaluated.

The validation reuses the probe result stored during provider `Configure` — no
additional HTTP call is made at plan time.

## Example Usage

```hcl
data "techzone_token_validation" "check" {}

resource "techzone_reservation" "example" {
  # Explicit dependency ensures the token is validated before any reservation
  # is created or read.
  depends_on = [data.techzone_token_validation.check]

  collection_id = "abc123def456789"
  user_email    = "rodolfo.castelo@hashicorp.com"
  hcp_org       = "org-AbCdEfGh"
  hcp_project   = "project-XxYyZz12"
}
```

## Argument Reference

This data source has no input arguments.

## Attribute Reference

The following computed attribute is exported:

* `status` - Always `"valid"` when the data source succeeds. If the token is
  expired or the TechZone API is unreachable, the plan fails before this
  attribute is set.
