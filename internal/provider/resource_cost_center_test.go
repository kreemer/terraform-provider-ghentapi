// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/kreemer/terraform-provider-ghentapi/internal/githubclient"
)

// mockCostCenter represents a single cost center in the mock server state.
type mockCostCenter struct {
	id                  string
	name                string
	state               string
	aiCreditPoolEnabled bool
	resources           []map[string]string // [{"type": "User", "name": "monalisa"}, ...]
}

// mockCostCenterServerState is shared mutable state for the cost center mock server.
type mockCostCenterServerState struct {
	mu      sync.Mutex
	centers map[string]*mockCostCenter
	nextID  int
}

// newCostCenterMockServer creates a mock server supporting the full cost
// center lifecycle, authenticated with the enterprise app only.
func newCostCenterMockServer(t *testing.T) (*httptest.Server, *mockCostCenterServerState) {
	t.Helper()
	state := &mockCostCenterServerState{
		centers: map[string]*mockCostCenter{},
		nextID:  1,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/app/installations/ent-install-id/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":      "ent-token",
				"expires_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
			})

		case r.URL.Path == "/app/installations/ent-install-id":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"account": map[string]string{"slug": "test-enterprise"},
			})

		case r.URL.Path == "/enterprises/test-enterprise/settings/billing/cost-centers" && r.Method == http.MethodPost:
			var body struct {
				Name                string `json:"name"`
				AICreditPoolEnabled bool   `json:"ai_credit_pool_enabled"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)

			state.mu.Lock()
			id := strconv.Itoa(state.nextID)
			state.nextID++
			cc := &mockCostCenter{
				id:                  id,
				name:                body.Name,
				state:               "active",
				aiCreditPoolEnabled: body.AICreditPoolEnabled,
				resources:           []map[string]string{},
			}
			state.centers[id] = cc
			state.mu.Unlock()

			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(mockCostCenterToJSON(cc))

		case strings.HasPrefix(r.URL.Path, "/enterprises/test-enterprise/settings/billing/cost-centers/"):
			rest := strings.TrimPrefix(r.URL.Path, "/enterprises/test-enterprise/settings/billing/cost-centers/")
			isResourcePath := strings.HasSuffix(rest, "/resource")
			id := strings.TrimSuffix(rest, "/resource")

			state.mu.Lock()
			cc, ok := state.centers[id]
			state.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "not found"})
				return
			}

			if isResourcePath {
				handleCostCenterResourceRequest(w, r, cc)
				return
			}

			switch r.Method {
			case http.MethodGet:
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(mockCostCenterToJSON(cc))

			case http.MethodPatch:
				var body struct {
					Name                *string `json:"name"`
					AICreditPoolEnabled *bool   `json:"ai_credit_pool_enabled"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				state.mu.Lock()
				if body.Name != nil {
					cc.name = *body.Name
				}
				if body.AICreditPoolEnabled != nil {
					cc.aiCreditPoolEnabled = *body.AICreditPoolEnabled
				}
				state.mu.Unlock()
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(mockCostCenterToJSON(cc))

			case http.MethodDelete:
				state.mu.Lock()
				cc.state = "deleted"
				state.mu.Unlock()
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"message":         "cost center archived",
					"id":              cc.id,
					"name":            cc.name,
					"costCenterState": "CostCenterArchived",
				})

			default:
				http.Error(w, "unexpected method "+r.Method, http.StatusMethodNotAllowed)
			}

		default:
			http.Error(w, fmt.Sprintf(`{"message":"not found: %s"}`, r.URL.Path), http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, state
}

// handleCostCenterResourceRequest implements POST/DELETE .../resource for a
// single cost center, guarded by the caller-held lack of lock (locks itself).
func handleCostCenterResourceRequest(w http.ResponseWriter, r *http.Request, cc *mockCostCenter) {
	var body struct {
		Users           []string `json:"users"`
		Organizations   []string `json:"organizations"`
		Repositories    []string `json:"repositories"`
		EnterpriseTeams []string `json:"enterprise_teams"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	switch r.Method {
	case http.MethodPost:
		for _, u := range body.Users {
			cc.resources = append(cc.resources, map[string]string{"type": "User", "name": u})
		}
		for _, o := range body.Organizations {
			cc.resources = append(cc.resources, map[string]string{"type": "Org", "name": o})
		}
		for _, rp := range body.Repositories {
			cc.resources = append(cc.resources, map[string]string{"type": "Repo", "name": rp})
		}
		for _, tm := range body.EnterpriseTeams {
			cc.resources = append(cc.resources, map[string]string{"type": "Team", "name": tm})
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "resources added", "reassigned_resources": nil})

	case http.MethodDelete:
		toRemove := map[string]struct{}{}
		for _, u := range body.Users {
			toRemove["User\x00"+u] = struct{}{}
		}
		for _, o := range body.Organizations {
			toRemove["Org\x00"+o] = struct{}{}
		}
		for _, rp := range body.Repositories {
			toRemove["Repo\x00"+rp] = struct{}{}
		}
		for _, tm := range body.EnterpriseTeams {
			toRemove["Team\x00"+tm] = struct{}{}
		}
		filtered := cc.resources[:0]
		for _, res := range cc.resources {
			key := res["type"] + "\x00" + res["name"]
			if _, remove := toRemove[key]; !remove {
				filtered = append(filtered, res)
			}
		}
		cc.resources = filtered
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "resources removed"})

	default:
		http.Error(w, "unexpected method "+r.Method, http.StatusMethodNotAllowed)
	}
}

func mockCostCenterToJSON(cc *mockCostCenter) map[string]any {
	return map[string]any{
		"id":                     cc.id,
		"name":                   cc.name,
		"state":                  cc.state,
		"ai_credit_pool_enabled": cc.aiCreditPoolEnabled,
		"resources":              cc.resources,
	}
}

func TestCostCenterResource_CreateRead(t *testing.T) {
	srv, _ := newCostCenterMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_cost_center" "test" {
  name = "Engineering"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("ghentapi_cost_center.test", "name", "Engineering"),
					resource.TestCheckResourceAttr("ghentapi_cost_center.test", "state", "active"),
					resource.TestCheckResourceAttr("ghentapi_cost_center.test", "ai_credit_pool_enabled", "false"),
					resource.TestCheckResourceAttrSet("ghentapi_cost_center.test", "id"),
				),
			},
		},
	})
}

func TestCostCenterResource_CreateWithResources(t *testing.T) {
	srv, _ := newCostCenterMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_cost_center" "test" {
  name  = "Engineering"
  users = ["monalisa"]
  organizations = ["my-org"]
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("ghentapi_cost_center.test", "users.#", "1"),
					resource.TestCheckTypeSetElemAttr("ghentapi_cost_center.test", "users.*", "monalisa"),
					resource.TestCheckResourceAttr("ghentapi_cost_center.test", "organizations.#", "1"),
					resource.TestCheckTypeSetElemAttr("ghentapi_cost_center.test", "organizations.*", "my-org"),
				),
			},
		},
	})
}

func TestCostCenterResource_Update(t *testing.T) {
	srv, _ := newCostCenterMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_cost_center" "test" {
  name = "Engineering"
}`,
				Check: resource.TestCheckResourceAttr("ghentapi_cost_center.test", "name", "Engineering"),
			},
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_cost_center" "test" {
  name                    = "Platform Engineering"
  ai_credit_pool_enabled  = true
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("ghentapi_cost_center.test", "name", "Platform Engineering"),
					resource.TestCheckResourceAttr("ghentapi_cost_center.test", "ai_credit_pool_enabled", "true"),
				),
			},
		},
	})
}

func TestCostCenterResource_ResourceAssignmentDiff(t *testing.T) {
	srv, _ := newCostCenterMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_cost_center" "test" {
  name  = "Engineering"
  users = ["monalisa", "octocat"]
}`,
				Check: resource.TestCheckResourceAttr("ghentapi_cost_center.test", "users.#", "2"),
			},
			{
				// octocat removed, hubot added.
				Config: providerConfig(srv.URL) + `
resource "ghentapi_cost_center" "test" {
  name  = "Engineering"
  users = ["monalisa", "hubot"]
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("ghentapi_cost_center.test", "users.#", "2"),
					resource.TestCheckTypeSetElemAttr("ghentapi_cost_center.test", "users.*", "monalisa"),
					resource.TestCheckTypeSetElemAttr("ghentapi_cost_center.test", "users.*", "hubot"),
				),
			},
		},
	})
}

func TestCostCenterResource_DriftDetection(t *testing.T) {
	srv, state := newCostCenterMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_cost_center" "test" {
  name = "Engineering"
}`,
				Check: resource.TestCheckResourceAttr("ghentapi_cost_center.test", "name", "Engineering"),
			},
			{
				PreConfig: func() {
					state.mu.Lock()
					for _, cc := range state.centers {
						cc.name = "Changed Outside Terraform"
					}
					state.mu.Unlock()
				},
				Config: providerConfig(srv.URL) + `
resource "ghentapi_cost_center" "test" {
  name = "Engineering"
}`,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

// TestBucketResourcesByType_RealAPITypeValues is a regression test for
// issue #13: GitHub's cost center API returns "Org"/"Repo" (not
// "Organization"/"Repository") in resources[].type, so any resource with
// these real-world values must be bucketed correctly instead of being
// silently dropped.
func TestBucketResourcesByType_RealAPITypeValues(t *testing.T) {
	resources := []githubclient.CostCenterResource{
		{Type: "User", Name: "monalisa"},
		{Type: "Org", Name: "my-org"},
		{Type: "Repo", Name: "octocat/hello-world"},
		{Type: "Team", Name: "my-team"},
		// Long-form aliases should also still be accepted defensively.
		{Type: "Organization", Name: "my-other-org"},
		{Type: "Repository", Name: "octocat/other-world"},
		{Type: "EnterpriseTeam", Name: "my-other-team"},
	}

	users, orgs, repos, teams := bucketResourcesByType(resources)

	if len(users) != 1 {
		t.Fatalf("expected 1 user, got %d: %v", len(users), users)
	}
	userValue, ok := users[0].(types.String)
	if !ok || userValue.ValueString() != "monalisa" {
		t.Errorf("expected users to contain monalisa, got %v", users)
	}
	if len(orgs) != 2 {
		t.Errorf("expected 2 organizations, got %d: %v", len(orgs), orgs)
	}
	if len(repos) != 2 {
		t.Errorf("expected 2 repositories, got %d: %v", len(repos), repos)
	}
	if len(teams) != 2 {
		t.Errorf("expected 2 enterprise teams, got %d: %v", len(teams), teams)
	}
}

func TestCostCenterResource_Import(t *testing.T) {
	srv, _ := newCostCenterMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_cost_center" "test" {
  name = "Engineering"
}`,
			},
			{
				ResourceName: "ghentapi_cost_center.test",
				Config: providerConfig(srv.URL) + `
resource "ghentapi_cost_center" "test" {
  name = "Engineering"
}`,
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs, ok := s.RootModule().Resources["ghentapi_cost_center.test"]
					if !ok {
						return "", fmt.Errorf("resource not found in state")
					}
					return rs.Primary.ID, nil
				},
			},
		},
	})
}
