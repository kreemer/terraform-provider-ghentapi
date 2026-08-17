// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package githubclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newOrgMembersMockServer creates a mock server handling both enterprise and
// org app token issuance plus the /api/graphql endpoint, so tests can assert
// which installation ID's token was actually used to authenticate the call.
func newOrgMembersMockServer(t *testing.T, graphqlHandler func(w http.ResponseWriter, authHeader string, body map[string]any)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v3/app/installations/ent-install-id/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "ent-token",
			"expires_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/api/v3/app/installations/ent-install-id", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"account": map[string]string{"slug": "test-enterprise"},
		})
	})
	mux.HandleFunc("/api/v3/enterprises/test-enterprise/apps/organizations/my-org/installations", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"installations": []map[string]any{
				{"id": 999, "app_id": "org-app-id", "client_id": "org-client-id"},
			},
		})
	})
	mux.HandleFunc("/api/v3/app/installations/999/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "org-token",
			"expires_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/api/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		graphqlHandler(w, r.Header.Get("Authorization"), body)
	})

	return httptest.NewServer(mux)
}

func TestClient_DoGraphQLWithOrgAuth_UsesOrgToken(t *testing.T) {
	var receivedAuth string

	srv := newOrgMembersMockServer(t, func(w http.ResponseWriter, authHeader string, _ map[string]any) {
		receivedAuth = authHeader
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"data":{"viewer":{"login":"someone"}}}`)
	})
	defer srv.Close()

	c := newTestClientWithGraphQL(srv.URL)
	_, err := c.DoGraphQLWithOrgAuth(context.Background(), "my-org", `query{viewer{login}}`, nil)
	if err != nil {
		t.Fatalf("DoGraphQLWithOrgAuth error: %v", err)
	}
	if receivedAuth != "token org-token" {
		t.Errorf("expected Authorization %q, got %q", "token org-token", receivedAuth)
	}
}

func TestClient_ListOrganizationMembers_SinglePage(t *testing.T) {
	srv := newOrgMembersMockServer(t, func(w http.ResponseWriter, _ string, _ map[string]any) {
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
	})
	defer srv.Close()

	c := newTestClientWithGraphQL(srv.URL)
	members, err := c.ListOrganizationMembers(context.Background(), "my-org")
	if err != nil {
		t.Fatalf("ListOrganizationMembers error: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}
	// Sorted by login: alice, bob
	if members[0].Login != "alice" || !members[0].IsOwner || members[0].Email != "alice@example.com" {
		t.Errorf("unexpected first member: %+v", members[0])
	}
	if members[1].Login != "bob" || members[1].IsOwner || members[1].Email != "" {
		t.Errorf("unexpected second member: %+v", members[1])
	}
}

func TestClient_ListOrganizationMembers_Pagination(t *testing.T) {
	callCount := 0

	srv := newOrgMembersMockServer(t, func(w http.ResponseWriter, _ string, body map[string]any) {
		callCount++
		vars, _ := body["variables"].(map[string]any)
		w.WriteHeader(http.StatusOK)

		if vars["after"] == nil {
			_, _ = fmt.Fprint(w, `{
				"data": {
					"organization": {
						"membersWithRole": {
							"edges": [
								{"role": "MEMBER", "node": {"login": "carol", "email": ""}}
							],
							"pageInfo": {"hasNextPage": true, "endCursor": "cursor-1"}
						}
					}
				}
			}`)
			return
		}

		if vars["after"] != "cursor-1" {
			t.Errorf("expected after=cursor-1, got %v", vars["after"])
		}
		_, _ = fmt.Fprint(w, `{
			"data": {
				"organization": {
					"membersWithRole": {
						"edges": [
							{"role": "ADMIN", "node": {"login": "dave", "email": "dave@example.com"}}
						],
						"pageInfo": {"hasNextPage": false, "endCursor": null}
					}
				}
			}
		}`)
	})
	defer srv.Close()

	c := newTestClientWithGraphQL(srv.URL)
	members, err := c.ListOrganizationMembers(context.Background(), "my-org")
	if err != nil {
		t.Fatalf("ListOrganizationMembers error: %v", err)
	}
	if callCount != 2 {
		t.Fatalf("expected 2 GraphQL calls (pagination), got %d", callCount)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members across pages, got %d", len(members))
	}
	// Sorted by login: carol, dave
	if members[0].Login != "carol" || members[1].Login != "dave" {
		t.Errorf("unexpected member order: %+v", members)
	}
	if !members[1].IsOwner {
		t.Errorf("expected dave to be owner")
	}
}

func TestClient_ListOrganizationMembers_Error(t *testing.T) {
	srv := newOrgMembersMockServer(t, func(w http.ResponseWriter, _ string, _ map[string]any) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"data":null,"errors":[{"message":"organization not found"}]}`)
	})
	defer srv.Close()

	c := newTestClientWithGraphQL(srv.URL)
	_, err := c.ListOrganizationMembers(context.Background(), "my-org")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "organization not found") {
		t.Errorf("expected error to contain 'organization not found', got %q", err.Error())
	}
}
