resource "ghentapi_enterprise_org" "example" {
  name          = "my-team"
  admin_logins  = ["alice", "bob"]
  billing_email = "billing@example.com"
  display_name  = "My Team"
}

# After the organisation exists, install the org app and manage settings.
resource "ghentapi_org_setting" "example" {
  organization = ghentapi_enterprise_org.example.name
  settings = {
    billing_email = "billing@example.com"
  }

  depends_on = [ghentapi_enterprise_org.example]
}

# Import an existing organisation that was not created by Terraform:
#   terraform import ghentapi_enterprise_org.example my-team
#
# Notes:
#   - If auto_install_org_app = true, the org app will be installed during import.
#   - admin_logins will be empty in state after import; run `terraform apply` once
#     to settle it to the value in your configuration.
import {
  to = ghentapi_enterprise_org.example
  id = "my-team"
}
