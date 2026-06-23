# Resource: techzone_reservation

Manages an IBM TechZone AWS account reservation. When applied, the provider
submits a reservation request to TechZone, polls until the reservation reaches
`Ready` status, and stores the resulting service links and dates in state.

## Key behaviors

- **Poll-to-Ready**: `Create` polls `GET /api/reservation/aws/<id>` every 10 seconds
  until the status is `Ready` or the `timeout_minutes` limit is reached.
- **Prune-on-expiry**: `Read` removes the resource from state automatically if the
  reservation returns HTTP 404, has a terminal status (`Deleted` or `Expired`), or
  has a `provisionUntil` date in the past. The next `apply` will recreate it.
- **Idempotent delete**: `Delete` treats HTTP 200, 204, and 404 as success.
  State is preserved (not cleared) on auth-failure status codes (302/401/403) so
  the operator can refresh their token and retry without re-importing.

## Example usage

```hcl
resource "techzone_reservation" "example" {
  collection_id = "abc123def456"
  user_email    = "user@example.com"
  hcp_org       = "org-XXXXXXXX"
  hcp_project   = "project-XXXXXXXX"

  # Optional — shown with non-default values
  template                 = "aws-account-hashicorp-ddr"
  region                   = "us-west-2"
  reservation_name         = "My Demo"
  purpose                  = "Demo"
  reservation_duration_days = 2
  timeout_minutes          = 45
}

output "aws_console_url" {
  value = one([
    for l in techzone_reservation.example.service_links : l.url
    if l.type == "AWS Console"
  ])
}
```

## Argument reference

### Required

| Argument | Type | Description |
|---|---|---|
| `collection_id` | String | TechZone collection ID for the reservation. |
| `user_email` | String | IBM ID (email) of the reservation owner. Used in the delete payload as `IBMID`. |
| `hcp_org` | String | HCP organization ID, injected as a dynamic output on the reservation. |
| `hcp_project` | String | HCP project ID, injected as a dynamic output on the reservation. |

### Optional

| Argument | Type | Default | Description |
|---|---|---|---|
| `template` | String | `aws-account-hashicorp-ddr` | TechZone template name. Changing this value forces replacement. |
| `region` | String | `us-east-2` | AWS region for the reservation. Changing this value forces replacement. |
| `reservation_name` | String | `Hashicorp DDR` | Display name for the reservation. Changing this value forces replacement. |
| `purpose` | String | `Demo` | Reservation purpose. Changing this value forces replacement. |
| `reservation_duration_days` | Int | `1` | Duration in days, applied at create time only. Changing this value **does not** modify the existing reservation; the new duration takes effect on the next recreate. |
| `timeout_minutes` | Int | `30` | Maximum minutes to wait for `Ready` status during create. Changing this value **does not** trigger replacement. |

## Attribute reference

| Attribute | Type | Description |
|---|---|---|
| `id` | String | TechZone reservation ID. |
| `status` | String | Current reservation status (e.g. `Ready`, `Provisioning`). |
| `service_links` | List of objects | Service links attached to the reservation. Each object has `type` (String) and `url` (String). |
| `start_date` | String | ISO-8601 provision start date, sourced from `provisionDate` (with fallback to `start` → `startDate`). |
| `end_date` | String | ISO-8601 provision end date, sourced from `provisionUntil` (with fallback to `end` → `endDate`). |

## Import

Existing reservations can be imported using the TechZone reservation ID:

```
terraform import techzone_reservation.example <reservation-id>
```

After import, run `terraform plan` to confirm the state matches the live reservation.

## Lifecycle notes

### Identity vs. operational attributes

**Identity attributes** — changes force resource replacement (destroy + create):

- `template`, `region`, `reservation_name`, `purpose`
- `collection_id`, `user_email`, `hcp_org`, `hcp_project`

**Operational attributes** — changes are applied in-place with no API calls:

- `reservation_duration_days`, `timeout_minutes`

### Prune rules

`Read` removes the resource from state (triggering recreate on next apply) when:

1. The API returns HTTP 404.
2. The reservation status is `Deleted` or `Expired`.
3. The `provisionUntil` date is in the past.

### Token expiry behavior

| Operation | Expired token behavior |
|---|---|
| `Create` | Fails immediately with `TECHZONE_API_KEY is invalid or expired`. |
| `Read` (during `terraform plan`) | Returns silently, leaving state unchanged, so `terraform destroy` can proceed. |
| `Delete` | Ignores the token probe result and attempts the delete. If TechZone returns 302/401/403, the error is surfaced and **state is preserved** so the operator can refresh the token and retry. |
