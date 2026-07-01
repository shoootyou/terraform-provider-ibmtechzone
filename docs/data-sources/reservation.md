# ibmtechzone_reservation (Data Source)

Reads an existing IBM TechZone reservation by ID. Use this data source for
read-only references to reservations that are managed outside of Terraform,
or to look up reservation outputs from a separately-managed resource.

-> **Note** For lifecycle management (create, update, destroy) use the
[ibmtechzone_reservation](../resources/reservation.md) resource instead.

## Example Usage

```hcl
data "ibmtechzone_reservation" "existing" {
  id = "6a3a88315f5d5f7d8f5aa373"
}

output "console_links" {
  value = data.ibmtechzone_reservation.existing.service_links
}
```

## Argument Reference

* `id` - (Required) The TechZone reservation ID to look up.

## Attribute Reference

In addition to the argument above, the following attributes are exported:

* `status` - Current reservation status (e.g. `Ready`, `Provisioning`).

* `service_links` - List of service link objects. Each object contains:
  * `type` - Service link type (e.g. `AWS Console`).
  * `url` - Service link URL.

* `start_date` - Reservation start date in ISO-8601 format, sourced from
  `provisionDate`.

* `end_date` - Reservation end date in ISO-8601 format, sourced from
  `provisionUntil`.
