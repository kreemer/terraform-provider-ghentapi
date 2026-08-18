resource "ghentapi_cost_center" "example" {
  name = "Engineering"

  # Optional: assign resources whose usage should be billed to this cost center.
  users            = ["monalisa"]
  organizations    = ["my-org"]
  repositories     = ["my-org/my-repo"]
  enterprise_teams = ["platform-team"]

  # Optional: draw from the AI credit pool instead of the shared enterprise
  # pool. Only allowed when the cost center contains only user/team resources.
  ai_credit_pool_enabled = false
}

# Import an existing cost center by its ID:
#   terraform import ghentapi_cost_center.example COST_CENTER_ID
#
# Note: GitHub provides no way to permanently delete a cost center.
# `terraform destroy` archives it (state becomes "deleted"); it still exists
# on GitHub.
import {
  to = ghentapi_cost_center.example
  id = "123"
}
