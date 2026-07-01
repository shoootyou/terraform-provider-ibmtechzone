# ibmtechzone Provider

Manages IBM TechZone AWS account reservations. The provider provisions temporary
AWS cloud accounts from the TechZone pool via the TechZone API.

-> **Note** This provider uses a user token from IBM TechZone. See the
[Authentication guide](guides/authentication.md) for how to obtain it.

## Example Usage

```hcl
terraform {
  required_providers {
    ibmtechzone = {
      source  = "registry.terraform.io/shoootyou/ibmtechzone"
      version = "~> 0.1"
    }
  }
}

provider "ibmtechzone" {
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

## Design Notes

Two design decisions from the Phase-1 template-agnostic refactor are worth
understanding before modifying the provider:

**The provider is template-agnostic.** At create time it calls
`GET /api/collection/<id>` and derives the `platform`, `template`,
`requestMethod`, `region`, `cloudAccount`, and `datacenter` fields directly
from the collection response — nothing is hardcoded per template. This means
the provider works with any collection the TechZone API returns, without
needing schema changes. The trade-off: the API call is access-gated. A token
that lacks read access to the collection receives a 401 or 403, which the
provider maps to "collection not found" (access-gated and non-existent are
indistinguishable from the API response).

**The `platform` block is re-emitted verbatim.** The value of `platforms[0]`
from the collection response is captured as raw JSON bytes and embedded
byte-identical into the POST payload via `json.RawMessage` — no re-encoding,
no field reordering, no normalization. This preserves whatever structure
TechZone expects, including any undocumented fields. The `template_variables`
map (TF attribute) holds opaque `{name, value}` pairs that the provider passes
through without introspection, emitting them as the `dynamicOutputs` wire field:
the `_NN_name` key convention is a TechZone contract, not a provider concern,
and template variables are not API-discoverable.
