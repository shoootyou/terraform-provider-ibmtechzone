# ibmtechzone_reservation (Resource)

Manages an IBM TechZone AWS account reservation. The provider fetches the
collection to derive the platform/region/template fields, submits a reservation
request, polls until the reservation reaches `Ready` status (configurable via
`timeout_minutes`), and stores the resulting service links and dates in state.

During `terraform refresh` and `terraform plan`, once the current time enters the
reservation's **extension window** — the last `extension_window_fraction` fraction
of `reservation_duration_days`, counted backward from `end_date` — the provider
first attempts to extend the reservation via the TechZone API (unless TechZone
already reports the reservation as terminally `Deleted`/`Expired`, in which case it
is pruned immediately, skipping the extension attempt). A successful extension
advances `end_date` and increments `extend_count` in state; no recreation happens
and no plan diff is produced. Extension is a best-effort optimization layered in
front of the resource's existing lifecycle: it never raises a blocking error, and
any failure to extend (API rejection, an exhausted extension limit, a transport
error, or simply being outside the window) falls back to the original behavior —
the resource prunes itself from state when the reservation has expired or been
deleted upstream, and the next `apply` recreates it. Extension attempts and
fallbacks are only visible via provider logs (`TF_LOG=INFO` or higher).

`Delete` is idempotent and preserves state on auth failure so the operator can
refresh their token and retry without re-importing.

## Example Usage

```hcl
data "ibmtechzone_token_validation" "check" {}

resource "ibmtechzone_reservation" "example" {
  depends_on = [data.ibmtechzone_token_validation.check]

  collection_id = "5f43a1b2c3d4e5f6a7b8c9d0"
  user_email    = "user@example.com"

  # template_variables: opaque _NN_name keys injected into the reservation payload.
  # Keys are mapped to the TechZone API's dynamicOutputs wire field, emitted in
  # lexicographic order as both a dynamicOutputs[] array and flat top-level keys
  # (dual-emit). An empty map {} is valid.
  template_variables = {
    "_03_account_cleanup" = "true"
    "_04_hcp_org"         = "org-AbCdEfGh"
    "_05_hcp_project"     = "project-XxYyZz12"
  }

  # requester_context: optional attribute for opportunity / account attribution.
  requester_context = {
    opportunity = ["006Ka00000NPHdITZSTG"]
    iui         = "ABC123DEF"
  }

  # Optional — shown with non-default values
  reservation_name          = "My Awesome Reservation"
  purpose                   = "Learning"
  reservation_duration_days = 2
  timeout_minutes           = 45
  extension_window_fraction = 0.25 # attempt extension in the last 12h of the 2-day duration
}

output "aws_console_url" {
  value = one([
    for l in ibmtechzone_reservation.example.service_links : l.url
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

* `template_variables` - (Required, Map of String) Opaque `_NN_name` output keys
  mapped to string values. These are injected into the TechZone reservation
  payload in two forms (mapped to the TechZone API's `dynamicOutputs` wire field):
  1. A `dynamicOutputs` array (elements in **lexicographic key order**).
  2. Flat top-level keys using the same names (dual-emit).

  An empty map (`{}`) is valid: it produces `"dynamicOutputs": []` and no flat
  keys. Keys conventionally follow the `_NN_name` pattern used by TechZone
  template variables (e.g. `_03_account_cleanup`, `_04_hcp_org`, `_05_hcp_project`),
  but the provider does not validate key format — the naming convention is enforced
  by TechZone, not the schema. Keys must not collide with reserved payload fields
  (the provider returns a plan-time error if they do).
  Changing this value forces a new resource.

* `requester_context` - (Optional, Object) Requester and account context for the
  reservation. Contains:
  * `opportunity` - (Optional, List of String) Salesforce opportunity IDs associated
    with this reservation (e.g. `["006Ka00000NPHdITZSTG"]`).
  * `iui` - (Optional, String) IBM User ID (IUI) for account attribution.

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

* `extension_window_fraction` - (Optional, Number) Fraction `(0, 1]` of
  `reservation_duration_days`, counted backward from `end_date`, during which the
  provider attempts to extend the reservation instead of recreating it (see the
  extension behavior described above). Defaults to `0.5` (the last 50% of the
  reservation's duration). Changing this value does not force a new resource.

  Validated at `terraform plan` time: `extension_window_fraction *
  reservation_duration_days` must not exceed `reservation_duration_days` —
  equivalent to requiring `extension_window_fraction <= 1.0` — so that a single
  successful extension always moves `end_date` back outside its own window.
  **Known limitation:** this cross-attribute check only runs when
  `reservation_duration_days` is *also* set explicitly in the same `resource`
  block. If `reservation_duration_days` is left at its own default, the validator
  has no resolved sibling value to compare against yet and silently skips the
  check — set both attributes explicitly if you depend on plan-time validation of
  `extension_window_fraction`.

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
  (with fallback to `end` → `endDate`). Advances automatically on each successful
  automatic extension (see `extension_window_fraction` above).

* `extend_count` - Number of successful extensions applied to this reservation via
  the automatic extension-window mechanism (wire field `extendCount`). Starts at
  `0` and resets to `0` when the resource is recreated — a new reservation begins
  a new count.

## Import

Reservations can be imported using the TechZone reservation ID:

```sh
terraform import ibmtechzone_reservation.example <reservation-id>
```

~> **Note** Importing a reservation only populates the `id` field. A subsequent
`terraform plan` will fetch the full state from the TechZone API.
