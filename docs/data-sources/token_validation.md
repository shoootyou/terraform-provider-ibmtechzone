# Data Source: techzone_token_validation

Validates that the provider's `api_key` is a valid, non-expired TechZone API
token. Returns `status = "valid"` when the token is accepted, or fails the
plan with an actionable error otherwise.

The validation reuses the probe result from provider `Configure` — no
additional HTTP call is made at plan time.

## Example usage

```hcl
data "techzone_token_validation" "check" {}

resource "techzone_reservation" "example" {
  # Explicit dependency ensures the token is validated before any reservation
  # is created or read.
  depends_on = [data.techzone_token_validation.check]

  collection_id = "abc123def456"
  user_email    = "user@example.com"
  hcp_org       = "org-XXXXXXXX"
  hcp_project   = "project-XXXXXXXX"
}
```

## Argument reference

This data source has no input arguments.

## Attribute reference

| Attribute | Type | Description |
|---|---|---|
| `status` | String | Always `"valid"` when the data source succeeds. If the token is expired or unreachable, the plan fails before this attribute is set. |

## When to use

Add `techzone_token_validation` to your configuration when you want the plan
to fail immediately with a clear, actionable error if the token is expired —
rather than having each `techzone_reservation` resource fail independently
during apply.

Recommended placement: declare it once at the module root and add
`depends_on = [data.techzone_token_validation.check]` to any
`techzone_reservation` resources.

If `TECHZONE_API_KEY` is invalid or expired, the data source emits one of
these errors:

- `TECHZONE_API_KEY is invalid or expired` — token rejected by TechZone.
  Refresh at [https://techzone.ibm.com](https://techzone.ibm.com) and re-run.
- `Could not reach TechZone API` — network or transport failure. Check
  connectivity and the `api_base` provider setting.
