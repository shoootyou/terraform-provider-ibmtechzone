---
page_title: "Authentication — TechZone API Token"
---

# Authentication

The TechZone provider authenticates using a browser-session bearer token issued
by IBM TechZone. There is no machine-credential or service-account mechanism —
the token is tied to a human IBM ID.

## Obtaining the token

1. Log in to [IBM TechZone](https://techzone.ibm.com) with your IBM ID.
2. Navigate to **My Profile** (top-right menu).
3. Copy the API token shown on your profile page.

~> **Warning** This token is a short-lived browser session credential. It expires
when your TechZone session ends or after a period of inactivity. You must refresh
it for each new Terraform run if the previous token has expired.

## Configuring the token

Set the token via environment variable (recommended — keeps it out of `.tf` files):

```sh
export TECHZONE_API_KEY="<your-techzone-token>"
```

Or pass it directly in the provider block (not recommended for shared configs):

```hcl
provider "techzone" {
  api_key = "<your-techzone-token>"
}
```

## Token validation

Use the `techzone_token_validation` data source as a plan-time gate to fail fast
if the token is expired before any reservation is created:

```hcl
data "techzone_token_validation" "check" {}

resource "techzone_reservation" "this" {
  depends_on = [data.techzone_token_validation.check]
  # ...
}
```

## Expired token behavior

| Operation | Behavior on expired token |
|-----------|--------------------------|
| `terraform plan` | Fails fast if `techzone_token_validation` data source is present |
| `terraform apply` (Create) | Returns error — reservation is not created |
| `terraform apply` (Read) | Silently skips refresh — state is preserved |
| `terraform destroy` (Delete) | Returns actionable error; state is preserved so retry is possible after token refresh |
