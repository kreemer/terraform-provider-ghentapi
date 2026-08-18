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

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// mockEnterpriseTeam represents a single enterprise team in the mock server state.
type mockEnterpriseTeam struct {
	id                        string
	name                      string
	description               string
	slug                      string
	organizationSelectionType string
	groupID                   string
	notificationSetting       string
}

// mockEnterpriseTeamServerState is shared mutable state for the enterprise
// team mock server, keyed by slug.
type mockEnterpriseTeamServerState struct {
	mu     sync.Mutex
	teams  map[string]*mockEnterpriseTeam
	nextID int
}

// enterpriseTeamSlug mimics GitHub's slug generation: lowercase, spaces to
// hyphens, prefixed with "ent:".
func enterpriseTeamSlug(name string) string {
	return "ent:" + strings.ReplaceAll(strings.ToLower(name), " ", "-")
}

// newEnterpriseTeamMockServer creates a mock server supporting the full
// enterprise team lifecycle, authenticated with the enterprise app only.
func newEnterpriseTeamMockServer(t *testing.T) (*httptest.Server, *mockEnterpriseTeamServerState) {
	t.Helper()
	state := &mockEnterpriseTeamServerState{
		teams:  map[string]*mockEnterpriseTeam{},
		nextID: 1,
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

		case r.URL.Path == "/enterprises/test-enterprise/teams" && r.Method == http.MethodPost:
			var body struct {
				Name                      string `json:"name"`
				Description               string `json:"description"`
				OrganizationSelectionType string `json:"organization_selection_type"`
				GroupID                   string `json:"group_id"`
				NotificationSetting       string `json:"notification_setting"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)

			state.mu.Lock()
			id := strconv.Itoa(state.nextID)
			state.nextID++
			team := &mockEnterpriseTeam{
				id:                        id,
				name:                      body.Name,
				description:               body.Description,
				slug:                      enterpriseTeamSlug(body.Name),
				organizationSelectionType: defaultString(body.OrganizationSelectionType, "disabled"),
				groupID:                   body.GroupID,
				notificationSetting:       defaultString(body.NotificationSetting, "notifications_enabled"),
			}
			state.teams[team.slug] = team
			state.mu.Unlock()

			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(mockEnterpriseTeamToJSON(team))

		case strings.HasPrefix(r.URL.Path, "/enterprises/test-enterprise/teams/"):
			slug := strings.TrimPrefix(r.URL.Path, "/enterprises/test-enterprise/teams/")

			state.mu.Lock()
			team, ok := state.teams[slug]
			state.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "not found"})
				return
			}

			switch r.Method {
			case http.MethodGet:
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(mockEnterpriseTeamToJSON(team))

			case http.MethodPatch:
				var body struct {
					Name                      *string `json:"name"`
					Description               *string `json:"description"`
					OrganizationSelectionType *string `json:"organization_selection_type"`
					GroupID                   *string `json:"group_id"`
					NotificationSetting       *string `json:"notification_setting"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				state.mu.Lock()
				delete(state.teams, slug)
				if body.Name != nil {
					team.name = *body.Name
					team.slug = enterpriseTeamSlug(*body.Name)
				}
				if body.Description != nil {
					team.description = *body.Description
				}
				if body.OrganizationSelectionType != nil {
					team.organizationSelectionType = *body.OrganizationSelectionType
				}
				if body.GroupID != nil {
					team.groupID = *body.GroupID
				}
				if body.NotificationSetting != nil {
					team.notificationSetting = *body.NotificationSetting
				}
				state.teams[team.slug] = team
				state.mu.Unlock()
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(mockEnterpriseTeamToJSON(team))

			case http.MethodDelete:
				state.mu.Lock()
				delete(state.teams, slug)
				state.mu.Unlock()
				w.WriteHeader(http.StatusNoContent)

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

func defaultString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func mockEnterpriseTeamToJSON(team *mockEnterpriseTeam) map[string]any {
	id, _ := strconv.ParseInt(team.id, 10, 64)
	return map[string]any{
		"id":                          id,
		"name":                        team.name,
		"description":                 team.description,
		"slug":                        team.slug,
		"organization_selection_type": team.organizationSelectionType,
		"group_id":                    team.groupID,
		"notification_setting":        team.notificationSetting,
	}
}

func TestEnterpriseTeamResource_CreateRead(t *testing.T) {
	srv, _ := newEnterpriseTeamMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_enterprise_team" "test" {
  name        = "Justice League"
  description = "A great team."
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("ghentapi_enterprise_team.test", "name", "Justice League"),
					resource.TestCheckResourceAttr("ghentapi_enterprise_team.test", "description", "A great team."),
					resource.TestCheckResourceAttr("ghentapi_enterprise_team.test", "slug", "ent:justice-league"),
					resource.TestCheckResourceAttr("ghentapi_enterprise_team.test", "organization_selection_type", "disabled"),
					resource.TestCheckResourceAttr("ghentapi_enterprise_team.test", "notification_setting", "notifications_enabled"),
					resource.TestCheckResourceAttrSet("ghentapi_enterprise_team.test", "id"),
				),
			},
		},
	})
}

func TestEnterpriseTeamResource_Update(t *testing.T) {
	srv, _ := newEnterpriseTeamMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_enterprise_team" "test" {
  name = "Justice League"
}`,
				Check: resource.TestCheckResourceAttr("ghentapi_enterprise_team.test", "slug", "ent:justice-league"),
			},
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_enterprise_team" "test" {
  name                  = "Renamed Team"
  notification_setting  = "notifications_disabled"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("ghentapi_enterprise_team.test", "name", "Renamed Team"),
					resource.TestCheckResourceAttr("ghentapi_enterprise_team.test", "slug", "ent:renamed-team"),
					resource.TestCheckResourceAttr("ghentapi_enterprise_team.test", "notification_setting", "notifications_disabled"),
				),
			},
		},
	})
}

func TestEnterpriseTeamResource_NotFoundRemovesFromState(t *testing.T) {
	srv, state := newEnterpriseTeamMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_enterprise_team" "test" {
  name = "Justice League"
}`,
				Check: resource.TestCheckResourceAttr("ghentapi_enterprise_team.test", "name", "Justice League"),
			},
			{
				PreConfig: func() {
					state.mu.Lock()
					for k := range state.teams {
						delete(state.teams, k)
					}
					state.mu.Unlock()
				},
				Config: providerConfig(srv.URL) + `
resource "ghentapi_enterprise_team" "test" {
  name = "Justice League"
}`,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestEnterpriseTeamResource_Import(t *testing.T) {
	srv, _ := newEnterpriseTeamMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_enterprise_team" "test" {
  name = "Justice League"
}`,
			},
			{
				ResourceName: "ghentapi_enterprise_team.test",
				Config: providerConfig(srv.URL) + `
resource "ghentapi_enterprise_team" "test" {
  name = "Justice League"
}`,
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs, ok := s.RootModule().Resources["ghentapi_enterprise_team.test"]
					if !ok {
						return "", fmt.Errorf("resource not found in state")
					}
					return rs.Primary.Attributes["slug"], nil
				},
			},
		},
	})
}

// TestEnterpriseTeamResource_ReferencedByCostCenter verifies that a
// ghentapi_cost_center can reference the slug of a ghentapi_enterprise_team
// via its enterprise_teams attribute.
func TestEnterpriseTeamResource_ReferencedByCostCenter(t *testing.T) {
	teamSrv, _ := newEnterpriseTeamMockServer(t)
	costCenterSrv, _ := newCostCenterMockServer(t)

	// Combine both mock servers into one, since both need to serve
	// requests behind a single base_url for this test.
	combined := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/enterprises/test-enterprise/teams") {
			teamSrv.Config.Handler.ServeHTTP(w, r)
			return
		}
		costCenterSrv.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(combined.Close)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(combined.URL) + `
resource "ghentapi_enterprise_team" "test" {
  name = "Platform Team"
}

resource "ghentapi_cost_center" "test" {
  name             = "Engineering"
  enterprise_teams = [ghentapi_enterprise_team.test.slug]
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("ghentapi_enterprise_team.test", "slug", "ent:platform-team"),
					resource.TestCheckResourceAttr("ghentapi_cost_center.test", "enterprise_teams.#", "1"),
					resource.TestCheckTypeSetElemAttr("ghentapi_cost_center.test", "enterprise_teams.*", "ent:platform-team"),
				),
			},
		},
	})
}
