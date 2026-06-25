---
page_title: "Authentication — TechZone API Token"
---

# Authentication

The TechZone provider authenticates using a user token issued by IBM TechZone. 

## Obtaining the token

1. Log in to [IBM TechZone](https://techzone.ibm.com) with your IBM ID.
2. Navigate to **My Profile** (top-right menu).
3. Copy the API token shown on your profile page.

## Configuring the token

Set the token via environment variable (recommended — keeps it out of `.tf` files):

```sh
export TECHZONE_API_KEY="<your-techzone-token>"
```

Or pass it directly in the provider block (not recommended for shared configs):

```hcl
provider "ibmtechzone" {
  api_key = "<your-techzone-token>"
}
```

## Token validation

Use the `ibmtechzone_token_validation` data source as a plan-time gate to fail fast
if the token is expired before any reservation is created:

```hcl
data "ibmtechzone_token_validation" "check" {}

resource "ibmtechzone_reservation" "this" {
  depends_on = [data.ibmtechzone_token_validation.check]
  # ...
}
```

## Expired token behavior

| Operation | Behavior on expired token |
|-----------|--------------------------|
| `terraform plan` | Fails fast if `ibmtechzone_token_validation` data source is present |
| `terraform apply` (Create) | Returns error — reservation is not created |
| `terraform apply` (Read) | Silently skips refresh — state is preserved |
| `terraform destroy` (Delete) | Returns actionable error; state is preserved so retry is possible after token refresh |
