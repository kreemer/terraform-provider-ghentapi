// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// mockOrg represents a single organisation in the mock server state.
type mockOrg struct {
	login           string
	billingEmail    string
	displayName     string
	nodeID          string
	installationID  int // org-app installation ID assigned at install time
}

// mockOrgServerState is shared mutable state for the enterprise org mock server.
type mockOrgServerState struct {
	mu            sync.Mutex
	orgs          map[string]*mockOrg
	nextInstallID int
}

// newEnterpriseOrgMockServer creates a mock server that supports the full
// enterprise org lifecycle:
//   - Enterprise installation token (ent-install-id)
//   - GraphQL: enterprise node ID + createEnterpriseOrganization mutation
//   - EnsureOrgInstallation: list + POST installs for each new org
//   - Org-app installation token per install ID
//   - REST GET/PATCH /orgs/{login} (accepts both enterprise and org tokens)
func newEnterpriseOrgMockServer(t *testing.T) (*httptest.Server, *mockOrgServerState) {
	t.Helper()
	state := &mockOrgServerState{
		orgs:          map[string]*mockOrg{},
		nextInstallID: 100,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		// ── enterprise installation token ──────────────────────────────────
		case r.URL.Path == "/app/installations/ent-install-id/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":      "ent-token",
				"expires_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
			})

		case r.URL.Path == "/app/installations/ent-install-id":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"account": map[string]string{"login": "test-enterprise"},
			})

		// ── org-app installation token (dynamic per install ID) ────────────
		case strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":      "org-token",
				"expires_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
			})

		// ── GraphQL ────────────────────────────────────────────────────────
		case r.URL.Path == "/graphql":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			query, _ := body["query"].(string)

			if strings.Contains(query, "enterprise(slug") {
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, `{"data":{"enterprise":{"id":"ENT_NODE_ID"}}}`)
				return
			}

			if strings.Contains(query, "createEnterpriseOrganization") {
				vars, _ := body["variables"].(map[string]any)
				input, _ := vars["input"].(map[string]any)
				login, _ := input["login"].(string)
				billing, _ := input["billingEmail"].(string)
				displayName, _ := input["profileName"].(string)

				if login == "" {
					w.WriteHeader(http.StatusOK)
					_, _ = fmt.Fprint(w, `{"errors":[{"message":"login is required"}]}`)
					return
				}

				org := &mockOrg{
					login:        login,
					billingEmail: billing,
					displayName:  displayName,
					nodeID:       "ORG_" + strings.ToUpper(login),
				}
				state.mu.Lock()
				state.orgs[login] = org
				state.mu.Unlock()

				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"createEnterpriseOrganization": map[string]any{
							"organization": map[string]string{
								"id":    org.nodeID,
								"login": login,
							},
						},
					},
				})
				return
			}

			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"errors":[{"message":"unknown query"}]}`)

		// ── EnsureOrgInstallation: list + install ─────────────────────────
		case strings.HasPrefix(r.URL.Path, "/enterprises/test-enterprise/apps/organizations/") &&
			strings.HasSuffix(r.URL.Path, "/installations"):

			orgLogin := strings.TrimSuffix(
				strings.TrimPrefix(r.URL.Path, "/enterprises/test-enterprise/apps/organizations/"),
				"/installations",
			)

			state.mu.Lock()
			org, exists := state.orgs[orgLogin]
			state.mu.Unlock()

			if r.Method == http.MethodGet {
				if !exists || org.installationID == 0 {
					// No installation yet.
					w.WriteHeader(http.StatusOK)
					_, _ = fmt.Fprint(w, "[]")
					return
				}
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"id": org.installationID, "client_id": "org-client-id"},
				})
				return
			}

			// POST — install the org app.
			if !exists {
				http.Error(w, `{"message":"org not found"}`, http.StatusNotFound)
				return
			}
			state.mu.Lock()
			state.nextInstallID++
			org.installationID = state.nextInstallID
			state.mu.Unlock()

			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": org.installationID})

		// ── REST /orgs/{login} ─────────────────────────────────────────────
		case strings.HasPrefix(r.URL.Path, "/orgs/"):
			parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/orgs/"), "/", 2)
			orgLogin := parts[0]

			state.mu.Lock()
			org, exists := state.orgs[orgLogin]
			state.mu.Unlock()

			switch r.Method {
			case http.MethodPatch:
				if !exists {
					http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
					return
				}
				var patch map[string]any
				_ = json.NewDecoder(r.Body).Decode(&patch)
				state.mu.Lock()
				if v, ok := patch["billing_email"].(string); ok {
					org.billingEmail = v
				}
				if v, ok := patch["name"].(string); ok {
					org.displayName = v
				}
				state.mu.Unlock()
				fallthrough

			case http.MethodGet:
				if !exists {
					http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
					return
				}
				state.mu.Lock()
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"login":         org.login,
					"name":          org.displayName,
					"billing_email": org.billingEmail,
				})
				state.mu.Unlock()

			default:
				http.Error(w, `{"message":"method not allowed"}`, http.StatusMethodNotAllowed)
			}

		default:
			http.Error(w, fmt.Sprintf(`{"message":"not found: %s"}`, r.URL.Path), http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, state
}

func TestEnterpriseOrgResource_CreateRead(t *testing.T) {
	srv, _ := newEnterpriseOrgMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_enterprise_org" "test" {
  name          = "new-org"
  admin_logins  = ["admin-user"]
  billing_email = "billing@example.com"
  display_name  = "New Organisation"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("ghentapi_enterprise_org.test", "name", "new-org"),
					resource.TestCheckResourceAttr("ghentapi_enterprise_org.test", "billing_email", "billing@example.com"),
					resource.TestCheckResourceAttr("ghentapi_enterprise_org.test", "display_name", "New Organisation"),
					resource.TestCheckResourceAttrSet("ghentapi_enterprise_org.test", "node_id"),
				),
			},
		},
	})
}

func TestEnterpriseOrgResource_Update(t *testing.T) {
	srv, _ := newEnterpriseOrgMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_enterprise_org" "test" {
  name          = "upd-org"
  admin_logins  = ["admin-user"]
  billing_email = "initial@example.com"
}`,
				Check: resource.TestCheckResourceAttr("ghentapi_enterprise_org.test", "billing_email", "initial@example.com"),
			},
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_enterprise_org" "test" {
  name          = "upd-org"
  admin_logins  = ["admin-user"]
  billing_email = "updated@example.com"
  display_name  = "Updated Name"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("ghentapi_enterprise_org.test", "billing_email", "updated@example.com"),
					resource.TestCheckResourceAttr("ghentapi_enterprise_org.test", "display_name", "Updated Name"),
				),
			},
		},
	})
}

func TestEnterpriseOrgResource_DeleteIsNoop(t *testing.T) {
	srv, state := newEnterpriseOrgMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_enterprise_org" "test" {
  name          = "del-org"
  admin_logins  = ["admin"]
  billing_email = "del@example.com"
}`,
				Check: resource.TestCheckResourceAttr("ghentapi_enterprise_org.test", "name", "del-org"),
			},
			{
				// Remove resource from config — triggers Terraform destroy.
				Config: providerConfig(srv.URL),
			},
		},
	})

	// After destroy, the org must still exist in mock state (no-op delete).
	state.mu.Lock()
	defer state.mu.Unlock()
	if _, ok := state.orgs["del-org"]; !ok {
		t.Error("expected org 'del-org' to still exist after Terraform destroy (no-op delete)")
	}
}

func TestEnterpriseOrgResource_NoDisplayName(t *testing.T) {
	srv, _ := newEnterpriseOrgMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_enterprise_org" "test" {
  name          = "minimal-org"
  admin_logins  = ["admin"]
  billing_email = "minimal@example.com"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("ghentapi_enterprise_org.test", "name", "minimal-org"),
					resource.TestCheckResourceAttr("ghentapi_enterprise_org.test", "billing_email", "minimal@example.com"),
					resource.TestCheckResourceAttr("ghentapi_enterprise_org.test", "display_name", ""),
				),
			},
		},
	})
}

// TestEnterpriseOrgResource_OrgAppInstalledAfterCreate verifies that after a
// Create, the org app installation is registered in the mock state (i.e.
// EnsureOrgInstallation was called).
func TestEnterpriseOrgResource_OrgAppInstalledAfterCreate(t *testing.T) {
	srv, state := newEnterpriseOrgMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_enterprise_org" "test" {
  name          = "install-check-org"
  admin_logins  = ["admin"]
  billing_email = "check@example.com"
}`,
				Check: resource.TestCheckResourceAttr("ghentapi_enterprise_org.test", "name", "install-check-org"),
			},
		},
	})

	state.mu.Lock()
	defer state.mu.Unlock()
	org, ok := state.orgs["install-check-org"]
	if !ok {
		t.Fatal("org not found in mock state")
	}
	if org.installationID == 0 {
		t.Error("expected org app installation ID to be set after Create, got 0")
	}
}

// TestEnterpriseOrgResource_Import verifies the full import lifecycle:
// 1. Pre-seed the mock server with an existing org (simulating an org that
//    exists on GitHub but is not yet in Terraform state).
// 2. Import by org name.
// 3. Verify billing_email and display_name are populated from the API.
// 4. Verify a subsequent apply with admin_logins in config settles without error.
func TestEnterpriseOrgResource_Import(t *testing.T) {
	srv, state := newEnterpriseOrgMockServer(t)

	// Pre-seed: org exists on GitHub but Terraform does not manage it yet.
	state.mu.Lock()
	state.orgs["import-org"] = &mockOrg{
		login:        "import-org",
		billingEmail: "import@example.com",
		displayName:  "Import Test Org",
		nodeID:       "ORG_IMPORT_ORG",
		// installationID = 0 means not yet installed — EnsureOrgInstallation
		// will install it when Read is called after import.
	}
	state.mu.Unlock()

	orgConfig := providerConfig(srv.URL) + `
resource "ghentapi_enterprise_org" "imported" {
  name          = "import-org"
  admin_logins  = ["alice"]
  billing_email = "import@example.com"
  display_name  = "Import Test Org"
}`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			// Step 1: import the existing org.
			{
				Config:            orgConfig,
				ResourceName:      "ghentapi_enterprise_org.imported",
				ImportState:       true,
				ImportStateId:     "import-org",
				ImportStateVerify: false, // admin_logins won't match (empty after import)
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("ghentapi_enterprise_org.imported", "name", "import-org"),
					resource.TestCheckResourceAttr("ghentapi_enterprise_org.imported", "billing_email", "import@example.com"),
					resource.TestCheckResourceAttr("ghentapi_enterprise_org.imported", "display_name", "Import Test Org"),
				),
			},
			// Step 2: apply with the full config to settle admin_logins.
			{
				Config: orgConfig,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("ghentapi_enterprise_org.imported", "name", "import-org"),
					resource.TestCheckResourceAttr("ghentapi_enterprise_org.imported", "billing_email", "import@example.com"),
					resource.TestCheckResourceAttr("ghentapi_enterprise_org.imported", "admin_logins.0", "alice"),
				),
			},
		},
	})

	// The org app must have been installed during import (EnsureOrgInstallation).
	state.mu.Lock()
	defer state.mu.Unlock()
	if org := state.orgs["import-org"]; org.installationID == 0 {
		t.Error("expected org app to be installed during import, installationID is still 0")
	}
}
