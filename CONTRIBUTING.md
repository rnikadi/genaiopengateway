# Contributing

Thanks for helping build GenAIOpenGateway.

## Developer Certificate of Origin

Every commit must be signed off to certify the [DCO](https://developercertificate.org/):

```
git commit -s -m "Your message"
```

This adds a `Signed-off-by: Your Name <you@example.com>` line. There is no CLA.

## Development

Requires Go 1.26 or newer.

```
make test   # run the test suite with the race detector
make vet
make run    # needs a config.yaml; start from config.example.yaml
```

To run the gateway with everything it talks to (Postgres, Valkey, an MCP server, and optionally a local model, Jaeger and Prometheus), use the [Docker Compose setup](deploy/compose/README.md): `cd deploy/compose && docker compose up -d` builds your checkout and starts it all.

Format code with `gofmt` before opening a pull request. CI runs gofmt, go vet, staticcheck, govulncheck, race-enabled tests and a build.

## Proposing larger changes

Open an issue describing the problem first. Changes to public APIs, the config format or the architecture need a short ADR in [`docs/adr/`](docs/adr/README.md).
