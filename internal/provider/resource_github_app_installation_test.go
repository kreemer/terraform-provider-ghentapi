// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// mockAppInstallation is a single installed GitHub App tracked by the mock server.
type mockAppInstallation struct {
	ID                  int64
	ClientID            string
	AppSlug             string
	RepositorySelection string
	Repositories        []string
}

// appInstallationState is the mutable state backing newGitHubAppInstallationMockServer.
type appInstallationState struct {
	mu            sync.Mutex
	nextID        int64
	installations map[string][]*mockAppInstallation // org -> installations
}

// newGitHubAppInstallationMockServer starts a mock GitHub API server that
// implements the enterprise-app-installation endpoints used by
// ghentapi_github_app_installation: enterprise slug resolution, list/install
// (POST is an upsert), and uninstall (DELETE).
func newGitHubAppInstallationMockServer(t *testing.T) (*httptest.Server, *appInstallationState) {
	t.Helper()
	state := &appInstallationState{
		nextID:        100,
		installations: make(map[string][]*mockAppInstallation),
	}

	mux := http.NewServeMux()

	mux.HandleFunc("POST /app/installations/ent-install-id/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "ent-token",
			"expires_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
		})
	})

	mux.HandleFunc("GET /app/installations/ent-install-id", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"account": map[string]string{"slug": "test-enterprise"},
		})
	})

	mux.HandleFunc("GET /enterprises/test-enterprise/apps/organizations/{org}/installations", func(w http.ResponseWriter, r *http.Request) {
		org := r.PathValue("org")
		state.mu.Lock()
		defer state.mu.Unlock()

		installations := state.installations[org]
		result := make([]map[string]interface{}, 0, len(installations))
		for _, inst := range installations {
			result = append(result, map[string]interface{}{
				"id":                   inst.ID,
				"client_id":            inst.ClientID,
				"app_slug":             inst.AppSlug,
				"repository_selection": inst.RepositorySelection,
			})
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(result)
	})

	mux.HandleFunc("POST /enterprises/test-enterprise/apps/organizations/{org}/installations", func(w http.ResponseWriter, r *http.Request) {
		org := r.PathValue("org")
		var body struct {
			ClientID            string   `json:"client_id"`
			RepositorySelection string   `json:"repository_selection"`
			Repositories        []string `json:"repositories"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		state.mu.Lock()
		defer state.mu.Unlock()

		var existing *mockAppInstallation
		for _, inst := range state.installations[org] {
			if inst.ClientID == body.ClientID {
				existing = inst
				break
			}
		}

		status := http.StatusCreated
		if existing != nil {
			existing.RepositorySelection = body.RepositorySelection
			existing.Repositories = body.Repositories
			status = http.StatusOK
		} else {
			state.nextID++
			existing = &mockAppInstallation{
				ID:                  state.nextID,
				ClientID:            body.ClientID,
				AppSlug:             "test-app-" + body.ClientID,
				RepositorySelection: body.RepositorySelection,
				Repositories:        body.Repositories,
			}
			state.installations[org] = append(state.installations[org], existing)
		}

		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":                   existing.ID,
			"client_id":            existing.ClientID,
			"app_slug":             existing.AppSlug,
			"repository_selection": existing.RepositorySelection,
		})
	})

	mux.HandleFunc("DELETE /enterprises/test-enterprise/apps/organizations/{org}/installations/{id}", func(w http.ResponseWriter, r *http.Request) {
		org := r.PathValue("org")
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)

		state.mu.Lock()
		defer state.mu.Unlock()

		installations := state.installations[org]
		for i, inst := range installations {
			if inst.ID == id {
				state.installations[org] = append(installations[:i], installations[i+1:]...)
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, fmt.Sprintf(`{"message":"not found: %s %s"}`, r.Method, r.URL.Path), http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, state
}

func TestGitHubAppInstallationResource_CreateRead_All(t *testing.T) {
	srv, _ := newGitHubAppInstallationMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_github_app_installation" "test" {
  organization = "my-org"
  client_id    = "Iv2abc123"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("ghentapi_github_app_installation.test", "organization", "my-org"),
					resource.TestCheckResourceAttr("ghentapi_github_app_installation.test", "client_id", "Iv2abc123"),
					resource.TestCheckResourceAttr("ghentapi_github_app_installation.test", "repository_selection", "all"),
					resource.TestCheckResourceAttr("ghentapi_github_app_installation.test", "app_slug", "test-app-Iv2abc123"),
					resource.TestCheckResourceAttrSet("ghentapi_github_app_installation.test", "installation_id"),
				),
			},
		},
	})
}

func TestGitHubAppInstallationResource_SelectedRepositories(t *testing.T) {
	srv, _ := newGitHubAppInstallationMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_github_app_installation" "test" {
  organization          = "my-org"
  client_id             = "Iv2abc123"
  repository_selection  = "selected"
  repositories          = ["repo-a", "repo-b"]
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("ghentapi_github_app_installation.test", "repository_selection", "selected"),
					resource.TestCheckResourceAttr("ghentapi_github_app_installation.test", "repositories.#", "2"),
				),
			},
		},
	})
}

func TestGitHubAppInstallationResource_MissingRepositoriesError(t *testing.T) {
	srv, _ := newGitHubAppInstallationMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_github_app_installation" "test" {
  organization         = "my-org"
  client_id            = "Iv2abc123"
  repository_selection = "selected"
}`,
				ExpectError: regexp.MustCompile(`(?i)repositories must be set`),
			},
		},
	})
}

func TestGitHubAppInstallationResource_Update(t *testing.T) {
	srv, _ := newGitHubAppInstallationMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_github_app_installation" "test" {
  organization = "my-org"
  client_id    = "Iv2abc123"
}`,
				Check: resource.TestCheckResourceAttr("ghentapi_github_app_installation.test", "repository_selection", "all"),
			},
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_github_app_installation" "test" {
  organization          = "my-org"
  client_id             = "Iv2abc123"
  repository_selection  = "selected"
  repositories          = ["repo-a"]
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("ghentapi_github_app_installation.test", "repository_selection", "selected"),
					resource.TestCheckResourceAttr("ghentapi_github_app_installation.test", "repositories.#", "1"),
				),
			},
		},
	})
}

func TestGitHubAppInstallationResource_DeleteUninstalls(t *testing.T) {
	srv, state := newGitHubAppInstallationMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_github_app_installation" "test" {
  organization = "my-org"
  client_id    = "Iv2abc123"
}`,
				Check: resource.TestCheckResourceAttrSet("ghentapi_github_app_installation.test", "installation_id"),
			},
		},
	})

	// resource.UnitTest automatically destroys resources created during the
	// test case once all steps complete; verify the mock server reflects that.
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.installations["my-org"]) != 0 {
		t.Fatalf("expected installation to be removed from mock server after destroy, got %+v", state.installations["my-org"])
	}
}

func TestGitHubAppInstallationResource_Import(t *testing.T) {
	srv, _ := newGitHubAppInstallationMockServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_github_app_installation" "test" {
  organization = "my-org"
  client_id    = "Iv2abc123"
}`,
			},
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_github_app_installation" "test" {
  organization = "my-org"
  client_id    = "Iv2abc123"
}`,
				ResourceName:                         "ghentapi_github_app_installation.test",
				ImportState:                          true,
				ImportStateId:                        "my-org/Iv2abc123",
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "installation_id",
			},
		},
	})
}
