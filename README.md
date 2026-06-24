# terraform-provider-ibmtechzone

Terraform provider for IBM TechZone AWS account reservations.

## Registry source

```hcl
terraform {
  required_providers {
    techzone = {
      source  = "shoootyou/ibmtechzone"
      version = "= 1.0"
    }
  }
}
```

## Quick start

```hcl
provider "techzone" {
  # api_key is read from TECHZONE_API_KEY if not set here.
}

resource "techzone_reservation" "example" {
  collection_id = "abc123"
  user_email    = "user@example.com"
  hcp_org       = "org-XXXXXXXX"
  hcp_project   = "project-XXXXXXXX"
}
```

Set the API key via environment variable:

```
export TECHZONE_API_KEY="<your-techzone-token>"
```

## Resources and data sources

- [techzone_reservation](docs/resources/reservation.md)
- [techzone_token_validation](docs/data-sources/token_validation.md)

## Contributing and releasing

See [docs/contributing/release.md](docs/contributing/release.md) for the automated release pipeline.

## License

[Mozilla Public License 2.0](LICENSE)
