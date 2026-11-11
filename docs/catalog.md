# Provider presets and price catalog

The gateway ships a versioned catalog of provider presets and model list prices, in [`internal/catalog/catalog.yaml`](../internal/catalog/catalog.yaml). The setup wizard fills in providers and models from it, and `GET /admin/v1/catalog` returns it for other tools.

Each preset has the provider type, base URL or cloud location, where to get a key, a link to the provider's price list, and its common models with their kind (chat, embeddings, image, speech, transcription, moderation, rerank, video or realtime) and price in the same fields as a target's `price` ([config.md](config.md)).

## Prices are a starting point

Prices are copied into each target when the wizard creates it, so they are ordinary config: change them on the Configuration page or in the config file, and nothing changes when the catalog does. They are each provider's public list price when the catalog version was written, without negotiated discounts, and providers change prices without notice, so check the linked price list before relying on cost reports.

The catalog is refreshed with each release. A test checks that every preset, with every model, is a config the gateway accepts.

## Adding a provider or model

Edit `internal/catalog/catalog.yaml` and open a pull request with a link to the provider's price list. Presets for any OpenAI-compatible service need only a `base_url`; set `custom_models: true` where users name their own models, such as deployments.
