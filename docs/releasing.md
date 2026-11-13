# Releases and versioning

## Versioning

Semantic versioning. Before 1.0, minor versions (0.x) may change config or admin API in documented ways; the model API follows the OpenAI and Anthropic contracts and does not break. From 1.0, breaking changes need a major version and a deprecation period of at least two minor releases.

## What a release contains

- Binaries for linux, darwin and windows on amd64 and arm64.
- Multi-arch container image at `ghcr.io/rnikadi/genaiopengateway`.
- SBOM (SPDX) for binaries and image.
- Cosign keyless signatures for binaries and image.
- Release notes generated from merged pull requests, with upgrade notes for any config change.

## Process

1. CI is green on `main`. CI already builds every binary and both image platforms.
2. Move the `Unreleased` notes in `CHANGELOG.md` under a `## [X.Y.Z] - date` heading, with upgrade notes for any config change, and merge that.
3. Push a `vX.Y.Z` tag on `main`, or run the **release** workflow from the Actions tab with the version, which tags the head of `main` itself. `.github/workflows/release.yml` refuses to publish without changelog notes for the version, then builds, signs and publishes. Versions with a suffix, such as `0.2.0-rc.1`, publish as prereleases.

Image tags are `X.Y.Z`, `X.Y` and `latest`.

## Verifying a release

Binaries: check the archive against `checksums.txt`, and the checksum file against its signature.

```
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/rnikadi/genaiopengateway/.github/workflows/release.yml@refs/(tags/v|heads/main$)' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --ignore-missing -c checksums.txt
```

Image:

```
cosign verify ghcr.io/rnikadi/genaiopengateway:X.Y.Z \
  --certificate-identity-regexp '^https://github.com/rnikadi/genaiopengateway/.github/workflows/release.yml@refs/(tags/v|heads/main$)' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The image carries an SPDX SBOM and SLSA provenance as attestations (`docker buildx imagetools inspect --format '{{ json .SBOM }}'`); binaries have `genaiopengateway_vX.Y.Z.spdx.json`.

## Cadence

A minor or patch release ships on the second Tuesday of each month when `main` has unreleased changes, so users can plan upgrades. Security fixes ship as patch releases whenever they are ready ([SECURITY.md](../SECURITY.md)). The steps above need no special access beyond write to the repository, so any maintainer can cut a release.

## Support

From 1.0.0, the latest minor release and the one before it receive bug and security fixes as patch releases. Every fourth minor release, starting with 1.0, is a long-term support (LTS) release that receives security fixes for 12 months. Releases before 1.0 receive no more fixes.

| Release | Released | Fixes until |
| --- | --- | --- |
| 1.0 (LTS) | 2026-11-12 | Security fixes until 2027-11-12; bug fixes until 1.2.0 is released |

### What is stable

From 1.0, these keep the promise in [Versioning](#versioning): no breaking change without a major version and two minor releases of deprecation.

- The model API: the OpenAI and Anthropic routes, and the provider relays (Gemini Live, Nova Sonic, Bedrock, realtime).
- The MCP and A2A endpoints.
- The admin API under `/admin/v1`, and the control plane API under `/cp/v1`.
- The config file format, and the stored config kinds.
- `gogctl` commands and flags, the Terraform provider's resources, the Helm chart's values and the CRDs at `genaiopengateway.io/v1`.
- Metric names and labels, and the JSON log fields.

Not covered: the admin UI's layout, the catalog's prices and presets (which change with providers'), the store's schema (migrated by the gateway), and anything documented as experimental.
