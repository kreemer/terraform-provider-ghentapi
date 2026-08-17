// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// newMockGitHubServerWithMembers extends the base mock GitHub server with a
// /graphql endpoint serving a fixed organization membership response.
func newMockGitHubServerWithMembers(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/app/installations/ent-install-id/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":      "ent-token",
				"expires_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
			})

		case "/app/installations/ent-install-id":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"account": map[string]string{"slug": "test-enterprise"},
			})

		case "/enterprises/test-enterprise/apps/organizations/my-org/installations":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": 42, "client_id": "org-client-id"},
			})

		case "/app/installations/42/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":      "org-token",
				"expires_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
			})

		case "/graphql":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{
				"data": {
					"organization": {
						"membersWithRole": {
							"edges": [
								{"role": "ADMIN", "node": {"login": "alice", "email": "alice@example.com"}},
								{"role": "MEMBER", "node": {"login": "bob", "email": ""}}
							],
							"pageInfo": {"hasNextPage": false, "endCursor": null}
						}
					}
				}
			}`)

		default:
			http.Error(w, fmt.Sprintf(`{"message":"not found: %s"}`, r.URL.Path), http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOrganizationMembersDataSource_Read(t *testing.T) {
	srv := newMockGitHubServerWithMembers(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
data "ghentapi_organization_members" "test" {
  organization = "my-org"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("data.ghentapi_organization_members.test", "organization", "my-org"),
					resource.TestCheckResourceAttr("data.ghentapi_organization_members.test", "members.#", "2"),
					resource.TestCheckResourceAttr("data.ghentapi_organization_members.test", "members.0.login", "alice"),
					resource.TestCheckResourceAttr("data.ghentapi_organization_members.test", "members.0.email", "alice@example.com"),
					resource.TestCheckResourceAttr("data.ghentapi_organization_members.test", "members.0.is_owner", "true"),
					resource.TestCheckResourceAttr("data.ghentapi_organization_members.test", "members.1.login", "bob"),
					resource.TestCheckResourceAttr("data.ghentapi_organization_members.test", "members.1.email", ""),
					resource.TestCheckResourceAttr("data.ghentapi_organization_members.test", "members.1.is_owner", "false"),
				),
			},
		},
	})
}

// TestOrganizationMembersDataSource_Schema verifies the schema attribute shape
// without requiring a running provider.
func TestOrganizationMembersDataSource_Schema(t *testing.T) {
	ds := &organizationMembersDataSource{}
	var resp datasource.SchemaResponse
	ds.Schema(context.Background(), datasource.SchemaRequest{}, &resp)

	if _, ok := resp.Schema.Attributes["organization"]; !ok {
		t.Fatal("organization attribute not found in schema")
	}
	membersAttr, ok := resp.Schema.Attributes["members"]
	if !ok {
		t.Fatal("members attribute not found in schema")
	}
	if !membersAttr.IsComputed() {
		t.Error("members attribute must be Computed")
	}
}
