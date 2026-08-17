variable "organizations" {
  type    = list(string)
  default = ["my-org"]
}

# Installs a user-specified GitHub App (identified by its client ID) into an
# organisation, authenticated with the enterprise-level GitHub App token.
# Unlike the provider's own implicit org app installation, destroying this
# resource actually uninstalls the app from the organisation.
resource "ghentapi_github_app_installation" "security_scanner" {
  for_each = toset(var.organizations)

  organization = each.key
  client_id    = "Iv2abc123aabbcc"

  repository_selection = "selected"
  repositories         = ["repo-a", "repo-b"]
}
