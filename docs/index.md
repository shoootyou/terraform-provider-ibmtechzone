# techzone Provider

Manages IBM TechZone AWS account reservations. The provider provisions temporary
AWS cloud accounts from the TechZone pool via the TechZone API.

-> **Note** This provider uses a user token from IBM TechZone. See the
[Authentication guide](guides/authentication.md) for how to obtain it.

## Example Usage

```hcl
terraform {
  required_providers {
    techzone = {
      source  = "app.terraform.io/hashicorp-ddr-platform-dev/techzone"
      version = "~> 1.0"
    }
  }
}

provider "techzone" {
  # api_key is read from TECHZONE_API_KEY if not set here
}
```

## Argument Reference

* `api_key` - (Optional) Bearer token for the TechZone API. Can also be set via the
  `TECHZONE_API_KEY` environment variable. Sensitive — never logged or included in
  diagnostics.

* `api_base` - (Optional) Base URL for the TechZone API. Must use `https://` unless
  the host is a loopback address (`127.0.0.1`, `localhost`, `::1`). Defaults to
  `https://api.techzone.ibm.com`.
