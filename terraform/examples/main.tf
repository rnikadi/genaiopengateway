terraform {
  required_providers {
    genaiopengateway = {
      source = "rnikadi/genaiopengateway"
    }
  }
}

# GOG_URL and GOG_ADMIN_TOKEN configure the provider when these are unset.
provider "genaiopengateway" {
  url = "https://gateway.example.com"
}

resource "genaiopengateway_config_object" "openai" {
  kind = "providers"
  name = "openai"
  spec = jsonencode({
    type     = "openai"
    base_url = "https://api.openai.com/v1"
    api_key  = var.openai_api_key
  })
}

resource "genaiopengateway_config_object" "gpt5_mini" {
  kind = "models"
  name = "gpt-5-mini"
  spec = jsonencode({
    targets = [{ provider = "openai", model = "gpt-5-mini" }]
  })
  depends_on = [genaiopengateway_config_object.openai]
}

resource "genaiopengateway_organization" "acme" {
  name   = "acme"
  budget = { amount_usd = 500, period = "monthly" }
}

resource "genaiopengateway_team" "search" {
  org_id         = genaiopengateway_organization.acme.id
  name           = "search"
  allowed_models = [genaiopengateway_config_object.gpt5_mini.name]
  rpm            = 600
}

resource "genaiopengateway_key" "search_prod" {
  team_id = genaiopengateway_team.search.id
  name    = "prod"
  owner   = "search-oncall@example.com"
}

resource "genaiopengateway_cedar_policies" "acme" {
  org_id = genaiopengateway_organization.acme.id
  text   = file("${path.module}/acme.cedar")
}

variable "openai_api_key" {
  type      = string
  sensitive = true
}

output "search_prod_key" {
  value     = genaiopengateway_key.search_prod.secret
  sensitive = true
}
