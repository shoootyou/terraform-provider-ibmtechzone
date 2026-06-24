# techzone_reservation (Resource)

Manages an IBM TechZone AWS account reservation. The provider fetches the
collection to derive the platform/region/template fields, submits a reservation
request, polls until the reservation reaches `Ready` status (configurable via
`timeout_minutes`), and stores the resulting service links and dates in state.

During `terraform refresh` and `terraform plan`, the resource prunes itself from
state automatically when the reservation has expired or been deleted upstream — the
next `apply` recreates it. `Delete` is idempotent and preserves state on auth
failure so the operator can refresh their token and retry without re-importing.

## Example Usage

```hcl
data "techzone_token_validation" "check" {}

resource "techzone_reservation" "example" {
  depends_on = [data.techzone_token_validation.check]

  collection_id = "5f43a1b2c3d4e5f6a7b8c9d0"
  user_email    = "user@example.com"

  # dynamic_outputs: opaque _NN_ keys injected into the reservation payload.
  # Keys are emitted in lexicographic order in both the dynamicOutputs[] array
  # and as flat top-level keys (dual-emit). An empty map {} is valid.
  dynamic_outputs = {
    "_04_hcp_org"     = "org-AbCdEfGh"
    "_05_hcp_project" = "project-XxYyZz12"
  }

  # Optional — shown with non-default values
  reservation_name          = "My Awesome Reservation"
  purpose                   = "Learning"
  reservation_duration_days = 2
  timeout_minutes           = 45
}

output "aws_console_url" {
  value = one([
    for l in techzone_reservation.example.service_links : l.url
    if l.type == "AWS Console"
  ])
}
```

## Argument Reference

The following arguments are supported:

* `collection_id` - (Required) TechZone collection ID for this reservation.
  Must be a **24-character hexadecimal string** (MongoDB ObjectID format,
  e.g. `5f43a1b2c3d4e5f6a7b8c9d0`). The schema validator enforces this format
  at plan time — an invalid value is rejected before any API call is made.

  The provider fetches `GET /api/collection/<id>` at create time to derive the
  platform, template, region, and cloud-account fields for the reservation
  payload. The token must have read access to the collection; 401/403 responses
  are treated as "not found" (access-gated collections are indistinguishable
  from non-existent ones). Changing this value forces a new resource.

* `user_email` - (Required) IBM ID (email address) of the reservation owner.
  Also used as the `IBMID` field in the delete payload.
  Changing this value forces a new resource.

* `dynamic_outputs` - (Required, Map of String) Opaque `_NN_name` output keys
  mapped to string values. These are injected into the TechZone reservation
  payload in two forms:
  1. A `dynamicOutputs` array (elements in **lexicographic key order**).
  2. Flat top-level keys using the same names (dual-emit).

  An empty map (`{}`) is valid: it produces `"dynamicOutputs": []` and no flat
  keys. Keys conventionally follow the `_NN_name` pattern used by TechZone
  dynamic outputs (e.g. `_04_hcp_org`, `_05_hcp_project`), but the provider
  does not validate key format — the naming convention is enforced by TechZone,
  not the schema. Keys must not collide with reserved payload fields (the
  provider returns a plan-time error if they do).
  Changing this value forces a new resource.

* `region` - (Optional) AWS region override. Defaults to `us-east-2`.
  When omitted, the default is applied automatically — no explicit value is
  required in config. Changing this value forces a new resource.

* `reservation_name` - (Optional) Display name for the reservation.
  Changing this value forces a new resource.

* `purpose` - (Optional) Reservation purpose.
  Changing this value forces a new resource.

* `reservation_duration_days` - (Optional, Number) Duration in days at create time.
  Defaults to `1`. Changing this value does **not** modify the existing reservation;
  the new duration applies on the next replacement.

* `timeout_minutes` - (Optional, Number) Maximum minutes to wait for the reservation
  to reach `Ready` status. Defaults to `30`. Changing this value does not force a
  new resource.

## Attribute Reference

In addition to all arguments above, the following computed attributes are exported:

* `id` - The TechZone reservation ID.

* `status` - Current reservation status (e.g. `Ready`, `Provisioning`).

* `service_links` - List of service link objects attached to the reservation.
  Each object contains:
  * `type` - Service link type (e.g. `AWS Console`).
  * `url` - Service link URL.

* `start_date` - Reservation start date (ISO-8601), sourced from `provisionDate`
  (with fallback to `start` → `startDate`).

* `end_date` - Reservation end date (ISO-8601), sourced from `provisionUntil`
  (with fallback to `end` → `endDate`).

## Import

Reservations can be imported using the TechZone reservation ID:

```sh
terraform import techzone_reservation.example <reservation-id>
```

~> **Note** Importing a reservation only populates the `id` field. A subsequent
`terraform plan` will fetch the full state from the TechZone API.
