# techzone_reservation (Resource)

Manages an IBM TechZone AWS account reservation. The provider submits a reservation
request to TechZone, polls until the reservation reaches `Ready` status (configurable
via `timeout_minutes`), and stores the resulting service links and dates in state.

During `terraform refresh` and `terraform plan`, the resource prunes itself from
state automatically when the reservation has expired or been deleted upstream — the
next `apply` recreates it. `Delete` is idempotent and preserves state on auth
failure so the operator can refresh their token and retry without re-importing.

## Example Usage

```hcl
data "techzone_token_validation" "check" {}

resource "techzone_reservation" "example" {
  depends_on = [data.techzone_token_validation.check]

  collection_id = "abc123def456789"
  user_email    = "rodolfo.castelo@hashicorp.com"
  hcp_org       = "org-AbCdEfGh"
  hcp_project   = "project-XxYyZz12"

  # Optional — shown with non-default values
  template                  = "aws-account-hashicorp-ddr"
  region                    = "us-west-2"
  reservation_name          = "DDR Demo — West"
  purpose                   = "Demo"
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
  Changing this value forces a new resource.

* `user_email` - (Required) IBM ID (email address) of the reservation owner.
  Also used as the `IBMID` field in the delete payload.
  Changing this value forces a new resource.

* `hcp_org` - (Required) HCP organization ID, injected as a dynamic output into
  the reservation. Changing this value forces a new resource.

* `hcp_project` - (Required) HCP project ID, injected as a dynamic output into
  the reservation. Changing this value forces a new resource.

* `template` - (Optional) TechZone template name. Defaults to
  `aws-account-hashicorp-ddr`. Changing this value forces a new resource.

* `region` - (Optional) AWS region for the reservation. Defaults to `us-east-2`.
  Changing this value forces a new resource.

* `reservation_name` - (Optional) Display name for the reservation. Defaults to
  `Hashicorp DDR`. Changing this value forces a new resource.

* `purpose` - (Optional) Reservation purpose. Defaults to `Demo`. Changing this
  value forces a new resource.

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
