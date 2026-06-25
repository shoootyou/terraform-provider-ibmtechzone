# terraform-provider-ibmtechzone

Terraform provider for IBM TechZone AWS account reservations.

## Registry source

```hcl
terraform {
  required_providers {
    ibmtechzone = {
      source  = "registry.terraform.io/shoootyou/ibmtechzone"
      version = "~> 0.1"
    }
  }
}
```

## Quick start

```hcl
provider "ibmtechzone" {
  # api_key is read from TECHZONE_API_KEY if not set here.
}

resource "ibmtechzone_reservation" "example" {
  collection_id = "5f43a1b2c3d4e5f6a7b8c9d0"
  user_email    = "user@example.com"

  template_variables = {
    "_03_account_cleanup" = "true"
    "_04_hcp_org"         = "org-XXXXXXXX"
    "_05_hcp_project"     = "project-XXXXXXXX"
  }
}
```

Set the API key via environment variable:

```
export TECHZONE_API_KEY="<your-techzone-token>"
```

## Resources and data sources

- [ibmtechzone_reservation](docs/resources/reservation.md)
- [ibmtechzone_token_validation](docs/data-sources/token_validation.md)

## Contributing and releasing

See [docs/contributing/release.md](docs/contributing/release.md) for the automated release pipeline.

## License

[Mozilla Public License 2.0](LICENSE)
