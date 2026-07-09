variable "organizations" {
  type    = list(string)
  default = ["my-org"]
}

# Obtain a short-lived installation token for each organisation.
# The token is generated fresh on every plan/apply and is never stored in state.
data "ghentapi_installation_token" "org" {
  for_each     = toset(var.organizations)
  organization = each.key
}

output "org_tokens" {
  value     = { for k, v in data.ghentapi_installation_token.org : k => v.token }
  sensitive = true
}
