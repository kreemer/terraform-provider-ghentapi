// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package githubclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient returns a Client pointed at baseURL with the test RSA key
// pre-loaded for both enterprise and org app credentials.
func newTestClient(baseURL string) *Client {
	return NewClient(ClientConfig{
		BaseURL:                     baseURL,
		EnterpriseAppID:             "ent-app-id",
		EnterpriseAppInstallationID: "ent-install-id",
		EnterpriseAppPEM:            []byte(testRSAKey),
		EnterpriseFineGrainedToken:  "ent-fine-grained-pat",
		OrgAppID:                    "org-app-id",
		OrgAppClientID:              "org-client-id",
		OrgAppPEM:                   []byte(testRSAKey),
		AutoInstall:                 true,
	})
}

func TestClient_Do_RetryOnServerError(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	resp, err := c.Do(context.Background(), http.MethodGet, "/test", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 calls (2 retries), got %d", calls.Load())
	}
}

func TestClient_Do_RetryExhausted(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	// Use a very short backoff by manipulating time — instead, we just verify
	// the error is returned. Actual back-off is 1s+2s+4s so we override the
	// httpClient timeout to make it fast enough for tests.
	c.httpClient = &http.Client{Timeout: 5 * time.Second}

	_, err := c.Do(context.Background(), http.MethodGet, "/fail", nil, nil)
	if err == nil {
		t.Fatal("expected error after retries exhausted, got nil")
	}
	// 1 initial + 3 retries = 4 total attempts
	if calls.Load() != 4 {
		t.Errorf("expected 4 attempts, got %d", calls.Load())
	}
}

func TestClient_Do_RetryOn429(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	resp, err := c.Do(context.Background(), http.MethodGet, "/rate", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 after retry, got %d", resp.StatusCode)
	}
}

func TestClient_Do_JSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "wrong content-type", http.StatusBadRequest)
			return
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if payload["key"] != "value" {
			http.Error(w, "unexpected payload", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	resp, err := c.Do(context.Background(), http.MethodPost, "/body", map[string]string{"key": "value"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 200, got %d: %s", resp.StatusCode, body)
	}
}

func TestClient_DoWithEnterpriseAuth_InjectsHeader(t *testing.T) {
	var authHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/ent-install-id/access_tokens" {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":      "ent-token-xyz",
				"expires_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
			})
			return
		}
		authHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	resp, err := c.DoWithEnterpriseAuth(context.Background(), http.MethodGet, "/some/api", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if authHeader != "token ent-token-xyz" {
		t.Errorf("expected Authorization: token ent-token-xyz, got %q", authHeader)
	}
}

func TestClient_DoWithEnterpriseFineGrainedAuth_InjectsHeader(t *testing.T) {
	var authHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	resp, err := c.DoWithEnterpriseFineGrainedAuth(context.Background(), http.MethodGet, "/some/api", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if authHeader != "Bearer ent-fine-grained-pat" {
		t.Errorf("expected Authorization: Bearer ent-fine-grained-pat, got %q", authHeader)
	}
}

func TestClient_DoWithEnterpriseFineGrainedAuth_ErrorsWhenTokenUnset(t *testing.T) {
	c := NewClient(ClientConfig{
		BaseURL:                     "http://example.invalid",
		EnterpriseAppID:             "ent-app-id",
		EnterpriseAppInstallationID: "ent-install-id",
		EnterpriseAppPEM:            []byte(testRSAKey),
	})

	_, err := c.DoWithEnterpriseFineGrainedAuth(context.Background(), http.MethodGet, "/some/api", nil)
	if err == nil {
		t.Fatal("expected error when enterprise_fine_grained_token is unset, got nil")
	}
	if !strings.Contains(err.Error(), "enterprise_fine_grained_token") {
		t.Errorf("expected error to mention enterprise_fine_grained_token, got: %v", err)
	}
}

func TestClient_ResolveEnterpriseSlug_UsesAppJWTNotInstallationToken(t *testing.T) {
	var installationInfoAuthHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/ent-install-id/access_tokens":
			// If resolveEnterpriseSlug ever exchanges for an installation
			// token, that would indicate the regression this test guards
			// against — the endpoint below must be called with the raw JWT.
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":      "ent-token-xyz",
				"expires_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
			})
		case "/app/installations/ent-install-id":
			installationInfoAuthHeader = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"account": map[string]string{"slug": "test-enterprise"},
			})
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	slug, err := c.resolveEnterpriseSlug(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slug != "test-enterprise" {
		t.Errorf("expected slug %q, got %q", "test-enterprise", slug)
	}

	if !strings.HasPrefix(installationInfoAuthHeader, "Bearer ") {
		t.Fatalf("expected Authorization header to be a Bearer JWT, got %q", installationInfoAuthHeader)
	}
	if installationInfoAuthHeader == "Bearer " {
		t.Fatal("expected a non-empty JWT after Bearer prefix")
	}
}

func TestClient_DoWithOrgAuth_InjectsHeader(t *testing.T) {
	var authHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/ent-install-id/access_tokens":
			// Enterprise installation token request.
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":      "ent-token-xyz",
				"expires_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
			})
		case "/app/installations/ent-install-id":
			// Enterprise installation info — returns the enterprise slug.
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"account": map[string]string{"slug": "test-enterprise"},
			})
		case "/enterprises/test-enterprise/apps/organizations/my-org/installations":
			// List org installations — return our org app installation.
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": 42, "client_id": "org-client-id"},
			})
		case "/app/installations/42/access_tokens":
			// Org installation token request.
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":      "org-token-abc",
				"expires_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
			})
		default:
			authHeader = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	resp, err := c.DoWithOrgAuth(context.Background(), "my-org", http.MethodGet, "/orgs/my-org", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if authHeader != "token org-token-abc" {
		t.Errorf("expected Authorization: token org-token-abc, got %q", authHeader)
	}
}

func TestClient_EnsureOrgInstallation_AutoInstall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/ent-install-id/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":      "ent-token-xyz",
				"expires_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
			})
		case "/app/installations/ent-install-id":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"account": map[string]string{"slug": "test-enterprise"},
			})
		case "/enterprises/test-enterprise/apps/organizations/new-org/installations":
			switch r.Method {
			case http.MethodGet:
				// Not installed yet — return empty list.
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, "[]")
			case http.MethodPost:
				// Install it.
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": 99})
			}
		}
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	id, err := c.EnsureOrgInstallation(context.Background(), "new-org")
	if err != nil {
		t.Fatalf("EnsureOrgInstallation error: %v", err)
	}
	if id != "99" {
		t.Errorf("expected installation ID 99, got %q", id)
	}
}

func TestClient_EnsureOrgInstallation_NoAutoInstall_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		case "/enterprises/test-enterprise/apps/organizations/uninstalled-org/installations":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "[]")
		}
	}))
	defer srv.Close()

	cfg := ClientConfig{
		BaseURL:                     srv.URL,
		EnterpriseAppID:             "ent-app-id",
		EnterpriseAppInstallationID: "ent-install-id",
		EnterpriseAppPEM:            []byte(testRSAKey),
		OrgAppID:                    "org-app-id",
		OrgAppClientID:              "org-client-id",
		OrgAppPEM:                   []byte(testRSAKey),
		AutoInstall:                 false,
	}
	c := NewClient(cfg)

	_, err := c.EnsureOrgInstallation(context.Background(), "uninstalled-org")
	if err == nil {
		t.Fatal("expected error when auto_install=false and app not installed, got nil")
	}
}

// enterpriseSlugHandlers returns the two mock endpoints needed to resolve the
// enterprise slug ("test-enterprise") in tests below.
func enterpriseSlugHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/app/installations/ent-install-id/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "ent-token-xyz",
			"expires_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/app/installations/ent-install-id", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"account": map[string]string{"slug": "test-enterprise"},
		})
	})
}

func TestClient_InstallGitHubApp_All(t *testing.T) {
	var gotBody map[string]interface{}

	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/apps/organizations/my-org/installations", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "unexpected method "+r.Method, http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":                   123,
			"app_slug":             "my-app",
			"client_id":            "Iv2abc123",
			"repository_selection": "all",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	installation, err := c.InstallGitHubApp(context.Background(), "my-org", "Iv2abc123", "all", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if installation.ID != "123" {
		t.Errorf("expected installation ID 123, got %q", installation.ID)
	}
	if installation.AppSlug != "my-app" {
		t.Errorf("expected app slug my-app, got %q", installation.AppSlug)
	}
	if installation.RepositorySelection != "all" {
		t.Errorf("expected repository_selection all, got %q", installation.RepositorySelection)
	}
	if gotBody["repositories"] != nil {
		t.Errorf("expected no repositories field to be sent for \"all\", got %v", gotBody["repositories"])
	}
	if gotBody["client_id"] != "Iv2abc123" {
		t.Errorf("expected client_id Iv2abc123 to be sent, got %v", gotBody["client_id"])
	}
}

func TestClient_InstallGitHubApp_SelectedRepositories(t *testing.T) {
	var gotBody map[string]interface{}

	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/apps/organizations/my-org/installations", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":                   124,
			"app_slug":             "my-app",
			"client_id":            "Iv2abc123",
			"repository_selection": "selected",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	installation, err := c.InstallGitHubApp(context.Background(), "my-org", "Iv2abc123", "selected", []string{"repo-a", "repo-b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if installation.RepositorySelection != "selected" {
		t.Errorf("expected repository_selection selected, got %q", installation.RepositorySelection)
	}
	repos, ok := gotBody["repositories"].([]interface{})
	if !ok || len(repos) != 2 {
		t.Fatalf("expected repositories [repo-a repo-b] to be sent, got %v", gotBody["repositories"])
	}
}

func TestClient_FindGitHubAppInstallation_Found(t *testing.T) {
	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/apps/organizations/my-org/installations", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode([]map[string]interface{}{
			{"id": 55, "client_id": "other-client", "app_slug": "other-app", "repository_selection": "all"},
			{"id": 56, "client_id": "Iv2abc123", "app_slug": "my-app", "repository_selection": "selected"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	installation, ok, err := c.FindGitHubAppInstallation(context.Background(), "my-org", "Iv2abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected installation to be found")
	}
	if installation.ID != "56" || installation.AppSlug != "my-app" {
		t.Errorf("unexpected installation: %+v", installation)
	}
}

func TestClient_FindGitHubAppInstallation_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/apps/organizations/my-org/installations", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "[]")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, ok, err := c.FindGitHubAppInstallation(context.Background(), "my-org", "Iv2abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected installation to not be found")
	}
}

func TestClient_FindGitHubAppInstallation_WrapperFormat(t *testing.T) {
	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/apps/organizations/my-org/installations", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"installations": []map[string]interface{}{
				{"id": 77, "client_id": "Iv2abc123", "app_slug": "my-app", "repository_selection": "none"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	installation, ok, err := c.FindGitHubAppInstallation(context.Background(), "my-org", "Iv2abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || installation.ID != "77" {
		t.Fatalf("expected installation 77 to be found via wrapper format, got ok=%v installation=%+v", ok, installation)
	}
}

func TestClient_UninstallGitHubApp_Success(t *testing.T) {
	var gotMethod, gotPath string

	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/apps/organizations/my-org/installations/56", func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	if err := c.UninstallGitHubApp(context.Background(), "my-org", "56"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", gotMethod)
	}
	if gotPath != "/enterprises/test-enterprise/apps/organizations/my-org/installations/56" {
		t.Errorf("unexpected path: %s", gotPath)
	}
}

func TestClient_UninstallGitHubApp_NotFoundTreatedAsSuccess(t *testing.T) {
	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/apps/organizations/my-org/installations/56", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	if err := c.UninstallGitHubApp(context.Background(), "my-org", "56"); err != nil {
		t.Fatalf("expected 404 to be treated as success, got error: %v", err)
	}
}

func TestClient_UninstallGitHubApp_ServerError(t *testing.T) {
	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/apps/organizations/my-org/installations/56", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"message":"forbidden"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	c.httpClient.Timeout = 5 * time.Second
	if err := c.UninstallGitHubApp(context.Background(), "my-org", "56"); err == nil {
		t.Fatal("expected error on 403 response, got nil")
	}
}

func TestClient_CreateCostCenter(t *testing.T) {
	var gotBody map[string]interface{}

	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/settings/billing/cost-centers", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "unexpected method "+r.Method, http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":                     "cc-1",
			"name":                   "Engineering",
			"state":                  "active",
			"ai_credit_pool_enabled": false,
			"resources":              []interface{}{},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	cc, err := c.CreateCostCenter(context.Background(), "Engineering", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cc.ID != "cc-1" || cc.Name != "Engineering" || cc.State != "active" {
		t.Errorf("unexpected cost center: %+v", cc)
	}
	if gotBody["name"] != "Engineering" {
		t.Errorf("expected name Engineering to be sent, got %v", gotBody["name"])
	}
	if gotBody["ai_credit_pool_enabled"] != false {
		t.Errorf("expected ai_credit_pool_enabled false to be sent, got %v", gotBody["ai_credit_pool_enabled"])
	}
}

func TestClient_GetCostCenter_Found(t *testing.T) {
	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/settings/billing/cost-centers/cc-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":                     "cc-1",
			"name":                   "Engineering",
			"state":                  "active",
			"ai_credit_pool_enabled": true,
			"resources": []map[string]string{
				{"type": "User", "name": "monalisa"},
				{"type": "Organization", "name": "my-org"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	cc, ok, err := c.GetCostCenter(context.Background(), "cc-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected cost center to be found")
	}
	if !cc.AICreditPoolEnabled {
		t.Errorf("expected AICreditPoolEnabled to be true")
	}
	if len(cc.Resources) != 2 {
		t.Fatalf("expected 2 resources, got %d", len(cc.Resources))
	}
}

func TestClient_GetCostCenter_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/settings/billing/cost-centers/missing", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, ok, err := c.GetCostCenter(context.Background(), "missing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected cost center to not be found")
	}
}

func TestClient_UpdateCostCenter(t *testing.T) {
	var gotBody map[string]interface{}

	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/settings/billing/cost-centers/cc-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "unexpected method "+r.Method, http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":                     "cc-1",
			"name":                   "New Name",
			"state":                  "active",
			"ai_credit_pool_enabled": true,
			"resources":              []interface{}{},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	name := "New Name"
	aiEnabled := true
	cc, err := c.UpdateCostCenter(context.Background(), "cc-1", &name, &aiEnabled)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cc.Name != "New Name" {
		t.Errorf("expected name New Name, got %q", cc.Name)
	}
	if gotBody["name"] != "New Name" {
		t.Errorf("expected name to be sent in patch body, got %v", gotBody["name"])
	}
}

func TestClient_DeleteCostCenter_Success(t *testing.T) {
	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/settings/billing/cost-centers/cc-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "unexpected method "+r.Method, http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"message":         "cost center archived",
			"id":              "cc-1",
			"name":            "Engineering",
			"costCenterState": "CostCenterArchived",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	if err := c.DeleteCostCenter(context.Background(), "cc-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClient_DeleteCostCenter_NotFoundTreatedAsSuccess(t *testing.T) {
	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/settings/billing/cost-centers/cc-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	if err := c.DeleteCostCenter(context.Background(), "cc-1"); err != nil {
		t.Fatalf("expected 404 to be treated as success, got error: %v", err)
	}
}

func TestClient_AddCostCenterResources(t *testing.T) {
	var gotBody map[string]interface{}

	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/settings/billing/cost-centers/cc-1/resource", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "unexpected method "+r.Method, http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"message":              "resources added",
			"reassigned_resources": nil,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	err := c.AddCostCenterResources(context.Background(), "cc-1", CostCenterResourceChanges{
		Users:         []string{"monalisa"},
		Organizations: []string{"my-org"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotBody["users"] == nil {
		t.Errorf("expected users to be sent, got %v", gotBody)
	}
	if gotBody["organizations"] == nil {
		t.Errorf("expected organizations to be sent, got %v", gotBody)
	}
	if gotBody["repositories"] != nil {
		t.Errorf("expected no repositories field when empty, got %v", gotBody["repositories"])
	}
}

func TestClient_AddCostCenterResources_EmptyIsNoop(t *testing.T) {
	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/settings/billing/cost-centers/cc-1/resource", func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("expected no request to be made for empty changes")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	if err := c.AddCostCenterResources(context.Background(), "cc-1", CostCenterResourceChanges{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClient_RemoveCostCenterResources(t *testing.T) {
	var gotBody map[string]interface{}

	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/settings/billing/cost-centers/cc-1/resource", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "unexpected method "+r.Method, http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "resources removed",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	err := c.RemoveCostCenterResources(context.Background(), "cc-1", CostCenterResourceChanges{
		Repositories:    []string{"my-repo"},
		EnterpriseTeams: []string{"platform-team"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotBody["repositories"] == nil {
		t.Errorf("expected repositories to be sent, got %v", gotBody)
	}
	if gotBody["enterprise_teams"] == nil {
		t.Errorf("expected enterprise_teams to be sent, got %v", gotBody)
	}
}

func TestClient_RemoveCostCenterResources_EmptyIsNoop(t *testing.T) {
	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/settings/billing/cost-centers/cc-1/resource", func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("expected no request to be made for empty changes")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	if err := c.RemoveCostCenterResources(context.Background(), "cc-1", CostCenterResourceChanges{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClient_CreateEnterpriseTeam(t *testing.T) {
	var gotBody map[string]interface{}

	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/teams", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "unexpected method "+r.Method, http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":                          1,
			"name":                        "Justice League",
			"description":                 "A great team.",
			"slug":                        "ent:justice-league",
			"organization_selection_type": "disabled",
			"group_id":                    nil,
			"notification_setting":        "notifications_enabled",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	team, err := c.CreateEnterpriseTeam(context.Background(), EnterpriseTeamInput{
		Name:        "Justice League",
		Description: "A great team.",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if team.ID != "1" || team.Slug != "ent:justice-league" || team.Name != "Justice League" {
		t.Errorf("unexpected enterprise team: %+v", team)
	}
	if gotBody["name"] != "Justice League" {
		t.Errorf("expected name to be sent, got %v", gotBody["name"])
	}
	if gotBody["description"] != "A great team." {
		t.Errorf("expected description to be sent, got %v", gotBody["description"])
	}
}

func TestClient_GetEnterpriseTeam_Found(t *testing.T) {
	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/teams/ent:justice-league", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":                          1,
			"name":                        "Justice League",
			"description":                 "A great team.",
			"slug":                        "ent:justice-league",
			"organization_selection_type": "all",
			"notification_setting":        "notifications_disabled",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	team, ok, err := c.GetEnterpriseTeam(context.Background(), "ent:justice-league")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected enterprise team to be found")
	}
	if team.OrganizationSelectionType != "all" {
		t.Errorf("expected organization_selection_type all, got %q", team.OrganizationSelectionType)
	}
	if team.NotificationSetting != "notifications_disabled" {
		t.Errorf("expected notification_setting notifications_disabled, got %q", team.NotificationSetting)
	}
}

func TestClient_GetEnterpriseTeam_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/teams/missing", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, ok, err := c.GetEnterpriseTeam(context.Background(), "missing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected enterprise team to not be found")
	}
}

func TestClient_UpdateEnterpriseTeam(t *testing.T) {
	var gotBody map[string]interface{}

	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/teams/ent:justice-league", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "unexpected method "+r.Method, http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":                          1,
			"name":                        "Renamed Team",
			"description":                 "A great team.",
			"slug":                        "ent:renamed-team",
			"organization_selection_type": "disabled",
			"notification_setting":        "notifications_enabled",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	team, err := c.UpdateEnterpriseTeam(context.Background(), "ent:justice-league", EnterpriseTeamInput{
		Name: "Renamed Team",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if team.Slug != "ent:renamed-team" {
		t.Errorf("expected new slug ent:renamed-team, got %q", team.Slug)
	}
	if gotBody["name"] != "Renamed Team" {
		t.Errorf("expected name to be sent in patch body, got %v", gotBody["name"])
	}
}

func TestClient_DeleteEnterpriseTeam_Success(t *testing.T) {
	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/teams/ent:justice-league", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "unexpected method "+r.Method, http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	if err := c.DeleteEnterpriseTeam(context.Background(), "ent:justice-league"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClient_DeleteEnterpriseTeam_NotFoundTreatedAsSuccess(t *testing.T) {
	mux := http.NewServeMux()
	enterpriseSlugHandlers(mux)
	mux.HandleFunc("/enterprises/test-enterprise/teams/ent:justice-league", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(srv.URL)
	if err := c.DeleteEnterpriseTeam(context.Background(), "ent:justice-league"); err != nil {
		t.Fatalf("expected 404 to be treated as success, got error: %v", err)
	}
}
