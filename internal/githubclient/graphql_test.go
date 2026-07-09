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

// newTestClientWithGraphQL returns a client pointed at the given server.
// The base URL is set to end with /api/v3 so graphqlURL() resolves correctly.
func newTestClientWithGraphQL(baseURL string) *Client {
	return NewClient(ClientConfig{
		BaseURL:                     baseURL + "/api/v3",
		EnterpriseAppID:             "ent-app-id",
		EnterpriseAppInstallationID: "ent-install-id",
		EnterpriseAppPEM:            []byte(testRSAKey),
		OrgAppID:                    "org-app-id",
		OrgAppClientID:              "org-client-id",
		OrgAppPEM:                   []byte(testRSAKey),
		AutoInstall:                 true,
	})
}

// newGraphQLMockServer creates a mock server that handles enterprise token
// requests and a /api/graphql endpoint.
func newGraphQLMockServer(t *testing.T, graphqlHandler func(w http.ResponseWriter, body map[string]any)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/api/v3/app/installations/ent-install-id/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":      "ent-token",
				"expires_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
			})

		case "/api/graphql":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			graphqlHandler(w, body)

		default:
			http.Error(w, fmt.Sprintf(`{"message":"not found: %s"}`, r.URL.Path), http.StatusNotFound)
		}
	}))
}

func TestClient_graphqlURL_GHES(t *testing.T) {
	c := &Client{cfg: ClientConfig{BaseURL: "https://github.example.com/api/v3"}}
	got := c.graphqlURL()
	want := "https://github.example.com/api/graphql"
	if got != want {
		t.Errorf("graphqlURL() = %q, want %q", got, want)
	}
}

func TestClient_graphqlURL_GHEC(t *testing.T) {
	c := &Client{cfg: ClientConfig{BaseURL: "https://api.github.com"}}
	got := c.graphqlURL()
	want := "https://api.github.com/graphql"
	if got != want {
		t.Errorf("graphqlURL() = %q, want %q", got, want)
	}
}

func TestClient_DoGraphQL_Success(t *testing.T) {
	var receivedQuery string
	var receivedVars map[string]any

	srv := newGraphQLMockServer(t, func(w http.ResponseWriter, body map[string]any) {
		receivedQuery, _ = body["query"].(string)
		receivedVars, _ = body["variables"].(map[string]any)
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"data":{"enterprise":{"id":"E_123"}}}`)
	})
	defer srv.Close()

	c := newTestClientWithGraphQL(srv.URL)
	data, err := c.DoGraphQL(context.Background(), `query($slug:String!){enterprise(slug:$slug){id}}`, map[string]any{"slug": "my-ent"})
	if err != nil {
		t.Fatalf("DoGraphQL error: %v", err)
	}

	if !strings.Contains(string(data), "E_123") {
		t.Errorf("expected E_123 in response data, got %s", string(data))
	}
	if !strings.Contains(receivedQuery, "enterprise") {
		t.Errorf("expected enterprise query to be forwarded, got %q", receivedQuery)
	}
	if receivedVars["slug"] != "my-ent" {
		t.Errorf("expected slug variable to be my-ent, got %v", receivedVars["slug"])
	}
}

func TestClient_DoGraphQL_GraphQLErrors(t *testing.T) {
	srv := newGraphQLMockServer(t, func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"data":null,"errors":[{"message":"not authorised"},{"message":"rate limited"}]}`)
	})
	defer srv.Close()

	c := newTestClientWithGraphQL(srv.URL)
	_, err := c.DoGraphQL(context.Background(), `query{viewer{login}}`, nil)
	if err == nil {
		t.Fatal("expected error from GraphQL errors, got nil")
	}
	if !strings.Contains(err.Error(), "not authorised") {
		t.Errorf("expected error to contain 'not authorised', got %q", err.Error())
	}
}

func TestClient_DoGraphQL_RetriesOnServerError(t *testing.T) {
	callCount := 0

	srv := newGraphQLMockServer(t, func(w http.ResponseWriter, _ map[string]any) {
		callCount++
		if callCount < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"data":{"ok":true}}`)
	})
	defer srv.Close()

	c := newTestClientWithGraphQL(srv.URL)
	_, err := c.DoGraphQL(context.Background(), `query{viewer{login}}`, nil)
	if err != nil {
		t.Fatalf("DoGraphQL error after retries: %v", err)
	}
	if callCount != 3 {
		t.Errorf("expected 3 calls (2 retries), got %d", callCount)
	}
}

func TestClient_resolveEnterpriseNodeID(t *testing.T) {
	srv := newGraphQLMockServer(t, func(w http.ResponseWriter, body map[string]any) {
		// The first call is resolveEnterpriseSlug (REST), the second is the node ID query.
		// However in this test the slug is fetched via a REST call first.
		// We just handle the GraphQL call here.
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"data":{"enterprise":{"id":"ENT_NODE_ID_42"}}}`)
	})
	defer srv.Close()

	// We need the enterprise slug REST endpoint too.
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
			"account": map[string]string{"login": "test-enterprise"},
		})
	})
	mux.HandleFunc("/api/graphql", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"data":{"enterprise":{"id":"ENT_NODE_ID_42"}}}`)
	})

	fullSrv := httptest.NewServer(mux)
	defer fullSrv.Close()

	c := newTestClientWithGraphQL(fullSrv.URL)
	nodeID, err := c.resolveEnterpriseNodeID(context.Background())
	if err != nil {
		t.Fatalf("resolveEnterpriseNodeID error: %v", err)
	}
	if nodeID != "ENT_NODE_ID_42" {
		t.Errorf("expected ENT_NODE_ID_42, got %q", nodeID)
	}

	// Second call should use the cache (no additional HTTP requests needed).
	nodeID2, err := c.resolveEnterpriseNodeID(context.Background())
	if err != nil {
		t.Fatalf("resolveEnterpriseNodeID (cached) error: %v", err)
	}
	if nodeID2 != nodeID {
		t.Errorf("cached node ID mismatch: %q vs %q", nodeID2, nodeID)
	}
}

func TestClient_CreateEnterpriseOrg(t *testing.T) {
	var mutationReceived map[string]any

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
			"account": map[string]string{"login": "test-enterprise"},
		})
	})
	mux.HandleFunc("/api/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		query, _ := body["query"].(string)

		if strings.Contains(query, "enterprise(slug") {
			// node ID query
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"data":{"enterprise":{"id":"ENT_NODE_ID"}}}`)
			return
		}

		// createEnterpriseOrganization mutation
		mutationReceived = body
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"data":{"createEnterpriseOrganization":{"organization":{"id":"ORG_NODE_ID","login":"my-new-org"}}}}`)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClientWithGraphQL(srv.URL)
	result, err := c.CreateEnterpriseOrg(context.Background(), EnterpriseOrgInput{
		Login:        "my-new-org",
		BillingEmail: "billing@example.com",
		AdminLogins:  []string{"admin-user"},
		DisplayName:  "My New Org",
	})
	if err != nil {
		t.Fatalf("CreateEnterpriseOrg error: %v", err)
	}
	if result.NodeID != "ORG_NODE_ID" {
		t.Errorf("expected NodeID ORG_NODE_ID, got %q", result.NodeID)
	}
	if result.Login != "my-new-org" {
		t.Errorf("expected Login my-new-org, got %q", result.Login)
	}

	// Verify the mutation variables were forwarded correctly.
	vars, _ := mutationReceived["variables"].(map[string]any)
	input, _ := vars["input"].(map[string]any)
	if input["login"] != "my-new-org" {
		t.Errorf("mutation input login = %v, want my-new-org", input["login"])
	}
	if input["billingEmail"] != "billing@example.com" {
		t.Errorf("mutation input billingEmail = %v, want billing@example.com", input["billingEmail"])
	}
	if input["enterpriseId"] != "ENT_NODE_ID" {
		t.Errorf("mutation input enterpriseId = %v, want ENT_NODE_ID", input["enterpriseId"])
	}
	if input["profileName"] != "My New Org" {
		t.Errorf("mutation input profileName = %v, want My New Org", input["profileName"])
	}
}

func TestClient_CreateEnterpriseOrg_NoDisplayName(t *testing.T) {
	var mutationInput map[string]any

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
			"account": map[string]string{"login": "test-enterprise"},
		})
	})
	mux.HandleFunc("/api/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		query, _ := body["query"].(string)
		if strings.Contains(query, "enterprise(slug") {
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"data":{"enterprise":{"id":"ENT_ID"}}}`)
			return
		}
		vars, _ := body["variables"].(map[string]any)
		mutationInput, _ = vars["input"].(map[string]any)
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"data":{"createEnterpriseOrganization":{"organization":{"id":"ORG_ID","login":"no-name-org"}}}}`)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClientWithGraphQL(srv.URL)
	_, err := c.CreateEnterpriseOrg(context.Background(), EnterpriseOrgInput{
		Login:        "no-name-org",
		BillingEmail: "billing@example.com",
		AdminLogins:  []string{"admin"},
		// DisplayName intentionally empty
	})
	if err != nil {
		t.Fatalf("CreateEnterpriseOrg error: %v", err)
	}

	// profileName should NOT be present in the mutation input when empty.
	if _, ok := mutationInput["profileName"]; ok {
		t.Error("profileName should not be sent when DisplayName is empty")
	}
}
