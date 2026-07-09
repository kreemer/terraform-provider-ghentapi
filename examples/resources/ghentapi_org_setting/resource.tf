variable "organizations" {
  type    = list(string)
  default = ["my-org"]
}

resource "ghentapi_org_setting" "this" {
  for_each = toset(var.organizations)

  organization = each.key

  # Only the keys listed here are managed; all other org settings are left untouched.
  settings = {
    billing_email = "github@example.com"
  }
}
