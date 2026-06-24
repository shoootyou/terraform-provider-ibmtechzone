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

## Setup (one-time)

These secrets must exist in the `deploy` GitHub Actions environment before any
release can run.

| Secret | Description |
|---|---|
| `GH_TOKEN` | Personal access token with `contents:write` and `pull-requests:write` scopes. Required because `GITHUB_TOKEN` cannot trigger downstream workflow runs. Used by `version.yml` (release-please) and `publish.yml` (asset download). |
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
| `terraform-provider-ibmtechzone_{version}_{os}_{arch}.zip` | Provider binary for each target platform (5 total) |
| `terraform-provider-ibmtechzone_{version}_SHA256SUMS` | SHA-256 checksums for all zips |
| `terraform-provider-ibmtechzone_{version}_SHA256SUMS.sig` | GPG detached signature over `SHA256SUMS` |
| `terraform-registry-manifest.json` | Registry protocol manifest (protocol `6.0`) |

**Target platforms:** `linux/amd64`, `linux/arm64`, `darwin/amd64`,
`darwin/arm64`, `windows/amd64`. `windows/arm64` is excluded — TFC runners
do not use it.
