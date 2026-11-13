# Security policy

GenAIOpenGateway holds every provider key a company uses, so security reports get priority over all other work.

## Reporting a vulnerability

Please do not report security issues in public issues, discussions or pull requests.

Report them privately through [GitHub security advisories](https://github.com/rnikadi/genaiopengateway/security/advisories/new). Include the version or commit, the configuration that matters, and steps to reproduce. You do not need a working exploit.

## What happens next

| Step | Target |
| --- | --- |
| Acknowledge the report | 3 business days |
| Confirm or dispute it, with a first severity (CVSS 4.0) | 7 days |
| Release a fix for a critical or high issue | 14 days from confirmation |
| Release a fix for a medium or low issue | The next scheduled release, at most 90 days |
| Publish the advisory and request a CVE | When the fix is released |

We disclose 90 days after the report at the latest, or sooner once a fix is out, and agree the date with the reporter. If an issue is being exploited in the wild, we release a fix and advisory as soon as one exists. Reporters are credited in the advisory unless they ask not to be.

Fixes ship as patch releases with signed binaries, images and charts ([docs/releasing.md](docs/releasing.md)). Subscribe to releases or to the repository's security advisories to hear about them.

## Supported versions

Until 1.0, only the latest minor release receives security fixes. From 1.0, the latest minor release and the previous one do, and long-term support releases are named in the [support policy](docs/releasing.md#support).

## Scope

In scope: the gateway binary, its container image, the Helm chart, the Compose files and the admin UI. Out of scope: vulnerabilities in upstream providers or MCP servers, denial of service by volume against an instance without rate limits configured, and the sample admin token in `deploy/compose`, which is public on purpose and only listens on 127.0.0.1.

## Hardening

The threat model and secure defaults are in [docs/security.md](docs/security.md).
