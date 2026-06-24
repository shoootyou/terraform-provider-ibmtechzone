# Changelog

All notable changes to `terraform-provider-techzone` are documented here.
Entries from v1.0.0 onward are generated automatically by [release-please](https://github.com/googleapis/release-please) from [Conventional Commits](https://www.conventionalcommits.org/).

<!-- release-please inserts new entries above this line -->

## [1.2.0](https://github.com/shoootyou/terraform-provider-techzone/compare/v1.1.0...v1.2.0) (2026-06-24)


### Features

* Adding personal GPG key support ([#3](https://github.com/shoootyou/terraform-provider-techzone/issues/3)) ([10657da](https://github.com/shoootyou/terraform-provider-techzone/commit/10657dae7c705a4a19c8bae74776669f0f35aef2))

## [1.1.0](https://github.com/shoootyou/terraform-provider-techzone/compare/v1.0.1...v1.1.0) (2026-06-24)


### Features

* All packages updated ([#1](https://github.com/shoootyou/terraform-provider-techzone/issues/1)) ([03dcd56](https://github.com/shoootyou/terraform-provider-techzone/commit/03dcd5627b153a772bb35c903391973ca9870b51))

## [1.0.1](https://github.com/shoootyou-ext/terraform-provider-techzone/compare/v1.0.0...v1.0.1) (2026-06-23)


### Bug Fixes

* add registry-compliant provider documentation ([e41a6fe](https://github.com/shoootyou-ext/terraform-provider-techzone/commit/e41a6feef4c3a813146b7cfcf4d72cba634b0be7))

## [1.0.0](https://github.com/shoootyou-ext/terraform-provider-techzone/compare/v0.4.0...v1.0.0) (2026-06-23)


### ⚠ BREAKING CHANGES

* provider now follows semantic versioning with automated releases via release-please. Manual tagging is no longer required.

### Features

* automated release pipeline + stable v1.0.0 ([834624b](https://github.com/shoootyou-ext/terraform-provider-techzone/commit/834624b87ae48c3009e050494540b20f04c06a02))

## [0.4.0] - 2026-06-23

### Fixed
- Poll loop used `GET /api/reservation/<id>` which does not exist — TechZone returns `302 → /api/reservation/unknown/<id>` for every request to that path. Fixed to `GET /api/reservation/aws/<id>`, matching the canonical final GET. This was the root cause of every Create timeout since v0.1.0.

## [0.3.0] - 2026-06-23

### Fixed
- Poll loop and final canonical GET no longer silently swallow errors. All retry paths (connectivity error, non-2xx HTTP status, JSON unmarshal failure, missing `status` field) now emit `tflog.Warn` diagnostics visible with `TF_LOG=warn`. Previously a 30-minute apply timeout produced zero diagnostic output, making root-cause analysis impossible.

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
