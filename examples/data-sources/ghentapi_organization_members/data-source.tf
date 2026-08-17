data "ghentapi_organization_members" "this" {
  organization = "my-org"
}

output "organization_members" {
  value = data.ghentapi_organization_members.this.members
}

# Filter for owners only.
output "organization_owners" {
  value = [
    for m in data.ghentapi_organization_members.this.members : m.login
    if m.is_owner
  ]
}
