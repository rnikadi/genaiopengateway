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

1. CI is green on `main`.
2. Update `CHANGELOG.md`.
3. Tag `vX.Y.Z` on `main`. The release workflow builds, signs and publishes.

## Support

Until 1.0, only the latest minor release receives fixes.
