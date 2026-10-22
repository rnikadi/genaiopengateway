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
3. Tag `vX.Y.Z` on `main` and push the tag. `.github/workflows/release.yml` refuses to publish without changelog notes for the version, then builds, signs and publishes. Tags with a suffix, such as `v0.2.0-rc.1`, publish as prereleases.

Image tags are `X.Y.Z`, `X.Y` and `latest`.

## Verifying a release

Binaries: check the archive against `checksums.txt`, and the checksum file against its signature.

```
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/rnikadi/genaiopengateway/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --ignore-missing -c checksums.txt
```

Image:

```
cosign verify ghcr.io/rnikadi/genaiopengateway:X.Y.Z \
  --certificate-identity-regexp '^https://github.com/rnikadi/genaiopengateway/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The image carries an SPDX SBOM and SLSA provenance as attestations (`docker buildx imagetools inspect --format '{{ json .SBOM }}'`); binaries have `genaiopengateway_vX.Y.Z.spdx.json`.

## Support

Until 1.0, only the latest minor release receives fixes.
