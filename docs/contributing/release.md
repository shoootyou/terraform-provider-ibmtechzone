# Automated release pipeline

## Overview

```
push to main
  └─► version.yml (release-please)
        └─► opens/updates Release PR
              └─► merge Release PR
                    ├─► release-please creates semver tag
                    └─► release.yml (GoReleaser) triggers on tag
                          └─► GitHub Release published
                                └─► [manual] publish.yml (workflow_dispatch)
                                      └─► uploads to TFC registry (org selectable)
```

All three workflows run in the `deploy` GitHub Actions environment.

## Versioning

Commit messages on `main` must follow [Conventional Commits](https://www.conventionalcommits.org/).
Release-please reads commit history to determine the next version and generate
`CHANGELOG.md` entries.

| Commit type | Version bump | Appears in changelog |
|---|---|---|
| `fix:` | Patch | Yes |
| `feat:` | Minor | Yes |
| `feat!:` or `BREAKING CHANGE:` footer | Major | Yes |
| `perf:`, `revert:` | Patch | Yes |
| `docs:` | None | Yes |
| `chore:`, `ci:`, `refactor:`, `test:`, `build:` | None | No |

A `!` suffix on any type (e.g. `fix!:`, `chore!:`) also triggers a major bump.

## Making a release

1. **Merge commits to `main`** using Conventional Commits. Each commit's type
   determines whether and how the version bumps.

2. **`version.yml` runs automatically** on every push to `main`. Release-please
   opens or updates a Release PR titled `chore(main): release X.Y.Z`. The PR
   body shows the generated `CHANGELOG.md` entry.

3. **Review the Release PR.** Verify the version number and changelog entry are
   correct. The PR also updates `.release-please-manifest.json`.

4. **Merge the Release PR.** Release-please creates the semver tag (e.g.
   `v0.5.0`). This tag push triggers `release.yml`.

5. **`release.yml` runs GoReleaser**, producing signed multi-arch binaries and
   uploading them to a GitHub Release.

6. **Publish to TFC manually.** After the GitHub Release is published by
   GoReleaser, go to **Actions → "Publish to TFC Registry" → Run workflow**.
   Provide two inputs:
   - **`release_tag`** — the tag just created (e.g. `v0.5.0`)
   - **`tfc_org`** — choose the target organization from the dropdown:
     - `hashicorp-ddr-platform-dev`
     - `hashicorp-ddr-platform-prod`
     - `hashicorp-wwtfo-demo-platform-dev`
     - `hashicorp-wwtfo-demo-platform-prod`

   The workflow can be re-run with the same `release_tag` and a different
   `tfc_org` to publish the same version to additional organizations.

7. **Verify:**
   - Check the [Actions tab](../../actions) for `Release` and `Publish to TFC Registry` runs.
   - Confirm the new version appears in the TFC registry:
     `https://app.terraform.io/app/hashicorp-ddr-platform-dev/registry/providers/private/hashicorp-ddr-platform-dev/techzone/`

## Setup (one-time)

These secrets must exist in the `deploy` GitHub Actions environment before any
release can run.

| Secret | Description |
|---|---|
| `GH_TOKEN` | Personal access token with `contents:write` and `pull-requests:write` scopes. Required because `GITHUB_TOKEN` cannot trigger downstream workflow runs. Used by `version.yml` (release-please) and `publish.yml` (asset download). |
| `TFC_TOKEN` | TFC API token for the `hashicorp-ddr-platform-dev` organization. Used by `publish.yml` to register the new version and upload platform binaries. |
| `GPG_PRIVATE_KEY` | Armored GPG private key used to sign `SHA256SUMS`. Already configured as a repo secret. |
| `GPG_FINGERPRINT` | Fingerprint of the signing key, passed to `gpg --local-user`. Already configured as a repo secret. |

To create the `deploy` environment: **Settings → Environments → New environment → `deploy`**.
Add the secrets above under the environment (not as repository-level secrets,
except `GPG_PRIVATE_KEY` and `GPG_FINGERPRINT` which are already repo secrets).

## Local development

```bash
make build      # compile provider binary for the local platform
make test       # unit tests — no live API, no TF_ACC required
make testacc    # acceptance tests against mock server (sets TF_ACC=1)
make lint       # run golangci-lint (must be installed separately)
```

The `testacc` target runs with a 120-minute timeout. Acceptance tests require
`TF_ACC=1`; without it they are silently skipped.

## GoReleaser artifacts

GoReleaser v2 produces the following artifacts per release. See
[`.goreleaser.yml`](../../.goreleaser.yml) for the full configuration.

| Artifact | Description |
|---|---|
| `terraform-provider-techzone_{version}_{os}_{arch}.zip` | Provider binary for each target platform (5 total) |
| `terraform-provider-techzone_{version}_SHA256SUMS` | SHA-256 checksums for all zips |
| `terraform-provider-techzone_{version}_SHA256SUMS.sig` | GPG detached signature over `SHA256SUMS` |
| `terraform-registry-manifest.json` | Registry protocol manifest (protocol `6.0`) |

**Target platforms:** `linux/amd64`, `linux/arm64`, `darwin/amd64`,
`darwin/arm64`, `windows/amd64`. `windows/arm64` is excluded — TFC runners
do not use it.

## TFC registry

The provider can be published to any of the four supported TFC organizations.
The `publish.yml` workflow accepts `tfc_org` as a dispatch input — select the
target organization at run time.

| Organization | Registry URL |
|---|---|
| `hashicorp-ddr-platform-dev` | `https://app.terraform.io/app/hashicorp-ddr-platform-dev/registry/providers/private/hashicorp-ddr-platform-dev/techzone/` |
| `hashicorp-ddr-platform-prod` | `https://app.terraform.io/app/hashicorp-ddr-platform-prod/registry/providers/private/hashicorp-ddr-platform-prod/techzone/` |
| `hashicorp-wwtfo-demo-platform-dev` | `https://app.terraform.io/app/hashicorp-wwtfo-demo-platform-dev/registry/providers/private/hashicorp-wwtfo-demo-platform-dev/techzone/` |
| `hashicorp-wwtfo-demo-platform-prod` | `https://app.terraform.io/app/hashicorp-wwtfo-demo-platform-prod/registry/providers/private/hashicorp-wwtfo-demo-platform-prod/techzone/` |

The provider source address pattern is:
`app.terraform.io/<tfc_org>/techzone`

After a successful `publish.yml` run, the new version is available immediately
for use in Terraform configurations via the `required_providers` source above.
