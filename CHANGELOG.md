# Changelog

All notable changes to `terraform-provider-techzone` are documented in this file.

## [0.2.0] - 2026-06-23

### Changed
- Output attributes (`service_links`, `start_date`, `end_date`) are no longer propagated as sensitive through the consuming module. These fields contain infrastructure metadata (ISO-8601 dates, service endpoint URLs) — not credentials. The provider schema never marked them sensitive; the explicit `sensitive = true` flags have been removed from the consuming module's outputs.
- Accompanies `ddr-cloudaccounts-temp-aws-account` module refactor: `techzone_reservation` resource now lives directly at the module root (the `modules/techzone-reservation/` submodule wrapper has been removed).

## [0.1.0] - 2026-06-23

### Added
- Initial release.
- `techzone_reservation` resource: full CRUD lifecycle (create with POST + poll-to-Ready, read with prune-on-expiry, replace-on-identity-change, idempotent delete).
- `techzone_token_validation` data source: plan-time token validity gate.
- Native port of the VCDLD-1678 token guard (status-wins, parseability check, loopback-aware HTTPS guard, bearer never logged).
- Prune/expiry logic: HTTP 404, terminal status (`Deleted`/`Expired`), and past `provisionUntil` all trigger recreate-on-next-apply.
- Delete returns an actionable error on auth-failure status codes (302/401/403) with state preserved for retry.
- GoReleaser v2: multi-arch (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64), GPG-signed SHA256SUMS, Terraform registry manifest (protocol 6.0).
