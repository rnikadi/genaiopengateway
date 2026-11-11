# Terraform provider

The `genaiopengateway` Terraform provider manages a gateway over its [admin API](api.md#admin-api), so organizations, teams, keys, Cedar policies, providers and models can live in the same Terraform code as the rest of your infrastructure. Its source is in [`terraform/`](https://github.com/rnikadi/genaiopengateway/tree/main/terraform). It is a separate Go module, and each release archive includes its binary.

For a plain YAML file in Git instead, see [`gogctl config import`](gogctl.md#configuration-as-code).

## Installing

Until the provider is on the Terraform Registry, install it from a release archive into a filesystem mirror:

```sh
VERSION=0.11.0 OS_ARCH=linux_amd64
dir=~/.terraform.d/plugins/registry.terraform.io/rnikadi/genaiopengateway/$VERSION/$OS_ARCH
mkdir -p "$dir"
cp terraform-provider-genaiopengateway "$dir/terraform-provider-genaiopengateway_v$VERSION"
```

Or build it from the repository with `cd terraform && go build`, and point Terraform at it in `~/.terraformrc`:

```hcl
provider_installation {
  dev_overrides { "rnikadi/genaiopengateway" = "/path/to/genaiopengateway/terraform" }
  direct {}
}
```

## Configuring

```hcl
terraform {
  required_providers {
    genaiopengateway = { source = "rnikadi/genaiopengateway" }
  }
}

provider "genaiopengateway" {
  url   = "https://gateway.example.com" # or GOG_URL
  token = var.admin_token               # or GOG_ADMIN_TOKEN
}
```

The token is the gateway's admin token, which acts as an owner. Every change is audited with actor `admin-token`.

## Resources

| Resource | Manages | Import id |
|---|---|---|
| `genaiopengateway_organization` | An organization and its policy | org id |
| `genaiopengateway_team` | A team of an organization and its policy | team id |
| `genaiopengateway_key` | A virtual key. `secret` holds the key, which the gateway shows only at creation. Destroying the key revokes it | key id; `secret` is empty after import |
| `genaiopengateway_cedar_policies` | An organization's [Cedar policies](cedar.md). Each change is a new version; destroying removes them | org id |
| `genaiopengateway_config_object` | A stored provider, model, guardrail, MCP server, virtual MCP server, alert webhook or stream, with `spec` in JSON as in the [config file](config.md) | `kind/name`, such as `models/gpt-5-mini` |

The data source `genaiopengateway_organization` finds an organization by name, for example one created in the admin UI.

Organizations, teams and keys take the policy settings of the admin API as attributes: `allowed_models`, `allowed_mcp_servers`, `rpm`, `tpm`, `guardrails`, `residency`, `log_bodies`, `budget = { amount_usd, period }` and `cache = { mode, ttl }`. A setting left out is no restriction, and removing one from the configuration clears it on the gateway. Keys also take `owner` and `expires_at`.

[`terraform/examples/main.tf`](https://github.com/rnikadi/genaiopengateway/blob/main/terraform/examples/main.tf) sets up a provider, a model, an organization with a team, a key and Cedar policies.

## Notes

- The gateway never returns secrets. A `config_object` compares its `spec` with the stored object with secrets masked, so a secret changed outside Terraform is not detected. Changing it in the configuration does update it.
- A key revoked outside Terraform is removed from the state, and the next apply creates a new one.
- A team with active keys cannot be destroyed. Revoke or destroy its keys first; Terraform does this itself when they are in the same configuration.
- Changes made in the admin UI show up as drift on the next plan.

## Testing

The provider's acceptance tests build the gateway, start it on SQLite and apply real configurations with the Terraform CLI:

```sh
cd terraform
TF_ACC=1 go test ./...
```
