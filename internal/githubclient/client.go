// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package githubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ClientConfig holds all configuration for constructing a Client.
type ClientConfig struct {
	BaseURL string

	// Enterprise GitHub App credentials — used to install the org app.
	EnterpriseAppID             string
	EnterpriseAppInstallationID string
	EnterpriseAppPEM            []byte

	// Org-level GitHub App credentials — used to manage org settings.
	OrgAppID            string
	OrgAppClientID      string
	OrgAppPEM           []byte
	RepositorySelection string // "all" or "selected"; defaults to "all"

	// AutoInstall controls whether the org app is installed automatically when
	// EnsureOrgInstallation is called for an org that does not have it yet.
	// When false, an error is returned instead.
	AutoInstall bool
}

// Client is an HTTP client that authenticates as a GitHub App and handles
// token lifecycle automatically. Tokens are cached in memory and never written
// to Terraform state.
type Client struct {
	cfg        ClientConfig
	httpClient *http.Client
	cache      *TokenCache

	// enterpriseSlug is resolved once from the enterprise installation info.
	enterpriseSlugOnce sync.Once
	enterpriseSlug     string
	enterpriseSlugErr  error

	// orgInstallMu guards orgInstallCache.
	orgInstallMu    sync.Mutex
	orgInstallCache map[string]string // org login → installation ID

	// enterpriseNodeID is resolved once via GraphQL from the enterprise slug.
	enterpriseNodeIDOnce sync.Once
	enterpriseNodeID     string
	enterpriseNodeIDErr  error
}

// NewClient constructs a Client from the given config. baseURL must not have a
// trailing slash. RepositorySelection defaults to "all" when empty.
func NewClient(cfg ClientConfig) *Client {
	if cfg.RepositorySelection == "" {
		cfg.RepositorySelection = "all"
	}
	return &Client{
		cfg:             cfg,
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		cache:           NewTokenCache(),
		orgInstallCache: make(map[string]string),
	}
}

// enterpriseToken returns a valid installation token for the enterprise app.
func (c *Client) enterpriseToken(ctx context.Context) (string, error) {
	return c.cache.Get(ctx, c.cfg.EnterpriseAppInstallationID, func() (string, time.Time, error) {
		appJWT, err := generateJWT(c.cfg.EnterpriseAppID, c.cfg.EnterpriseAppPEM)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("enterprise JWT: %w", err)
		}
		return getInstallationToken(ctx, c.cfg.BaseURL, c.cfg.EnterpriseAppInstallationID, appJWT)
	})
}

// orgToken returns a valid installation token for the given org installation.
func (c *Client) orgToken(ctx context.Context, installationID string) (string, error) {
	return c.cache.Get(ctx, installationID, func() (string, time.Time, error) {
		appJWT, err := generateJWT(c.cfg.OrgAppID, c.cfg.OrgAppPEM)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("org JWT: %w", err)
		}
		return getInstallationToken(ctx, c.cfg.BaseURL, installationID, appJWT)
	})
}

// resolveEnterpriseSlug fetches the enterprise slug from the enterprise app
// installation info. The result is cached after the first successful call.
func (c *Client) resolveEnterpriseSlug(ctx context.Context) (string, error) {
	c.enterpriseSlugOnce.Do(func() {
		path := fmt.Sprintf("/app/installations/%s", c.cfg.EnterpriseAppInstallationID)
		resp, err := c.DoWithEnterpriseAuth(ctx, http.MethodGet, path, nil)
		if err != nil {
			c.enterpriseSlugErr = fmt.Errorf("fetching enterprise installation info: %w", err)
			return
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			c.enterpriseSlugErr = fmt.Errorf("reading enterprise installation response: %w", err)
			return
		}
		if resp.StatusCode != http.StatusOK {
			c.enterpriseSlugErr = fmt.Errorf("enterprise installation request failed (status %d): %s", resp.StatusCode, string(body))
			return
		}

		var result struct {
			Account struct {
				Login string `json:"login"`
			} `json:"account"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			c.enterpriseSlugErr = fmt.Errorf("decoding enterprise installation response: %w", err)
			return
		}
		if result.Account.Login == "" {
			c.enterpriseSlugErr = fmt.Errorf("enterprise installation response missing account.login")
			return
		}
		c.enterpriseSlug = result.Account.Login
	})
	return c.enterpriseSlug, c.enterpriseSlugErr
}

// EnsureOrgInstallation returns the installation ID for the org app in the
// given organisation. It first checks an in-memory cache, then queries the
// GitHub API. If the app is not installed and AutoInstall is true it installs
// it automatically; otherwise it returns an error.
func (c *Client) EnsureOrgInstallation(ctx context.Context, org string) (string, error) {
	c.orgInstallMu.Lock()
	if id, ok := c.orgInstallCache[org]; ok {
		c.orgInstallMu.Unlock()
		return id, nil
	}
	c.orgInstallMu.Unlock()

	slug, err := c.resolveEnterpriseSlug(ctx)
	if err != nil {
		return "", err
	}

	// List current installations for the org and look for our app client ID.
	installID, err := c.findOrgInstallation(ctx, slug, org)
	if err != nil {
		return "", err
	}

	if installID != "" {
		c.orgInstallMu.Lock()
		c.orgInstallCache[org] = installID
		c.orgInstallMu.Unlock()
		return installID, nil
	}

	// Not installed.
	if !c.cfg.AutoInstall {
		return "", fmt.Errorf(
			"org app is not installed in organisation %q and auto_install_org_app is disabled; "+
				"install the app manually or set auto_install_org_app = true in the provider configuration",
			org,
		)
	}

	installID, err = c.installOrgApp(ctx, slug, org)
	if err != nil {
		return "", err
	}

	c.orgInstallMu.Lock()
	c.orgInstallCache[org] = installID
	c.orgInstallMu.Unlock()
	return installID, nil
}

type orgInstallation struct {
	ID      int64  `json:"id"`
	AppSlug string `json:"app_slug"`
	Account struct {
		Login string `json:"login"`
	} `json:"account"`
	ClientID string `json:"client_id"`
}

// findOrgInstallation searches the enterprise API for an existing installation
// of our org app in the given organisation. Returns "" if not found.
func (c *Client) findOrgInstallation(ctx context.Context, enterpriseSlug, org string) (string, error) {
	path := fmt.Sprintf("/enterprises/%s/apps/organizations/%s/installations", enterpriseSlug, org)
	resp, err := c.DoWithEnterpriseAuth(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", fmt.Errorf("listing org installations: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading org installations response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("listing org installations failed (status %d): %s", resp.StatusCode, string(body))
	}

	var installations []orgInstallation
	if err := json.Unmarshal(body, &installations); err != nil {
		// Some API versions wrap the list; try the wrapper form.
		var wrapper struct {
			Installations []orgInstallation `json:"installations"`
		}
		if err2 := json.Unmarshal(body, &wrapper); err2 != nil {
			return "", fmt.Errorf("decoding org installations: %w", err)
		}
		installations = wrapper.Installations
	}

	for _, inst := range installations {
		if inst.ClientID == c.cfg.OrgAppClientID {
			return fmt.Sprintf("%d", inst.ID), nil
		}
	}
	return "", nil
}

// installOrgApp calls the enterprise API to install the org app into the org.
func (c *Client) installOrgApp(ctx context.Context, enterpriseSlug, org string) (string, error) {
	path := fmt.Sprintf("/enterprises/%s/apps/organizations/%s/installations", enterpriseSlug, org)
	payload := map[string]string{
		"client_id":            c.cfg.OrgAppClientID,
		"repository_selection": c.cfg.RepositorySelection,
	}
	resp, err := c.DoWithEnterpriseAuth(ctx, http.MethodPost, path, payload)
	if err != nil {
		return "", fmt.Errorf("installing org app: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading install response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("installing org app failed (status %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decoding install response: %w", err)
	}
	return fmt.Sprintf("%d", result.ID), nil
}

// OrgToken returns a valid installation token for the given org. It calls
// EnsureOrgInstallation to resolve the installation ID first.
func (c *Client) OrgToken(ctx context.Context, org string) (string, error) {
	installID, err := c.EnsureOrgInstallation(ctx, org)
	if err != nil {
		return "", err
	}
	return c.orgToken(ctx, installID)
}

// Do executes an HTTP request against the provider base URL. If body is
// non-nil it is serialised as JSON. On HTTP 429 or 5xx the request is retried
// up to 3 times with exponential back-off (1 s, 2 s, 4 s).
func (c *Client) Do(ctx context.Context, method, path string, body interface{}, extraHeaders map[string]string) (*http.Response, error) {
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshalling request body: %w", err)
		}
	}

	backoff := time.Second
	var lastErr error

	for attempt := 0; attempt <= 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		var bodyReader io.Reader
		if bodyBytes != nil {
			bodyReader = bytes.NewReader(bodyBytes)
		}

		req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, bodyReader)
		if err != nil {
			return nil, fmt.Errorf("building request: %w", err)
		}

		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		for k, v := range extraHeaders {
			req.Header.Set(k, v)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("executing request: %w", err)
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			// Drain body so the connection can be reused.
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("request failed with status %d", resp.StatusCode)
			continue
		}

		return resp, nil
	}

	return nil, fmt.Errorf("request failed after retries: %w", lastErr)
}

// DoWithEnterpriseAuth executes a request authenticated with the enterprise
// app installation token.
func (c *Client) DoWithEnterpriseAuth(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	tok, err := c.enterpriseToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("obtaining enterprise token: %w", err)
	}
	return c.Do(ctx, method, path, body, map[string]string{
		"Authorization": "token " + tok,
	})
}

// DoWithOrgAuth executes a request authenticated with the org app installation
// token for the given organisation.
func (c *Client) DoWithOrgAuth(ctx context.Context, org, method, path string, body interface{}) (*http.Response, error) {
	tok, err := c.OrgToken(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("obtaining org token for %q: %w", org, err)
	}
	return c.Do(ctx, method, path, body, map[string]string{
		"Authorization": "token " + tok,
	})
}

// graphqlURL derives the GraphQL endpoint URL from the configured REST base URL.
// For GHES the REST base is  .../api/v3  and GraphQL lives at  .../api/graphql.
// For GHEC the REST base is  https://api.github.com  and GraphQL at  .../graphql.
func (c *Client) graphqlURL() string {
	if strings.HasSuffix(c.cfg.BaseURL, "/api/v3") {
		return strings.TrimSuffix(c.cfg.BaseURL, "/api/v3") + "/api/graphql"
	}
	return c.cfg.BaseURL + "/graphql"
}

// DoGraphQL executes a GraphQL query or mutation authenticated as the enterprise
// app. Retries are performed for HTTP 429 and 5xx responses (same policy as Do).
func (c *Client) DoGraphQL(ctx context.Context, query string, variables map[string]any) (json.RawMessage, error) {
	tok, err := c.enterpriseToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("obtaining enterprise token for GraphQL: %w", err)
	}

	payload := map[string]any{"query": query, "variables": variables}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshalling GraphQL request: %w", err)
	}

	backoff := time.Second
	var lastErr error

	for attempt := 0; attempt <= 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.graphqlURL(), bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, fmt.Errorf("building GraphQL request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "token "+tok)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("executing GraphQL request: %w", err)
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("GraphQL request failed with status %d", resp.StatusCode)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading GraphQL response: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GraphQL request failed (status %d): %s", resp.StatusCode, string(body))
		}

		var result struct {
			Data   json.RawMessage `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("decoding GraphQL response: %w", err)
		}
		if len(result.Errors) > 0 {
			msgs := make([]string, len(result.Errors))
			for i, e := range result.Errors {
				msgs[i] = e.Message
			}
			return nil, fmt.Errorf("GraphQL errors: %s", strings.Join(msgs, "; "))
		}
		return result.Data, nil
	}

	return nil, fmt.Errorf("GraphQL request failed after retries: %w", lastErr)
}

// resolveEnterpriseNodeID returns the GraphQL node ID of the enterprise account.
// The slug is resolved via resolveEnterpriseSlug. Result is cached.
func (c *Client) resolveEnterpriseNodeID(ctx context.Context) (string, error) {
	c.enterpriseNodeIDOnce.Do(func() {
		slug, err := c.resolveEnterpriseSlug(ctx)
		if err != nil {
			c.enterpriseNodeIDErr = err
			return
		}
		const q = `query($slug: String!) { enterprise(slug: $slug) { id } }`
		data, err := c.DoGraphQL(ctx, q, map[string]any{"slug": slug})
		if err != nil {
			c.enterpriseNodeIDErr = fmt.Errorf("resolving enterprise node ID: %w", err)
			return
		}
		var result struct {
			Enterprise struct {
				ID string `json:"id"`
			} `json:"enterprise"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			c.enterpriseNodeIDErr = fmt.Errorf("decoding enterprise node ID response: %w", err)
			return
		}
		if result.Enterprise.ID == "" {
			c.enterpriseNodeIDErr = fmt.Errorf("enterprise node ID not found in GraphQL response")
			return
		}
		c.enterpriseNodeID = result.Enterprise.ID
	})
	return c.enterpriseNodeID, c.enterpriseNodeIDErr
}

// EnterpriseOrgInput holds the input parameters for creating an enterprise organisation.
type EnterpriseOrgInput struct {
	Login        string
	BillingEmail string
	AdminLogins  []string
	DisplayName  string // optional
}

// EnterpriseOrgResult holds the result of a successful organisation creation.
type EnterpriseOrgResult struct {
	NodeID string
	Login  string
}

// CreateEnterpriseOrg creates a new organisation within the enterprise using
// the GraphQL createEnterpriseOrganization mutation authenticated as the
// enterprise app. It does NOT install the org app into the new organisation.
func (c *Client) CreateEnterpriseOrg(ctx context.Context, input EnterpriseOrgInput) (EnterpriseOrgResult, error) {
	enterpriseID, err := c.resolveEnterpriseNodeID(ctx)
	if err != nil {
		return EnterpriseOrgResult{}, fmt.Errorf("resolving enterprise node ID: %w", err)
	}

	const mutation = `
mutation CreateOrg($input: CreateEnterpriseOrganizationInput!) {
  createEnterpriseOrganization(input: $input) {
    organization {
      id
      login
    }
  }
}`

	inputVars := map[string]any{
		"enterpriseId": enterpriseID,
		"login":        input.Login,
		"billingEmail": input.BillingEmail,
		"adminLogins":  input.AdminLogins,
	}
	if input.DisplayName != "" {
		inputVars["profileName"] = input.DisplayName
	}

	data, err := c.DoGraphQL(ctx, mutation, map[string]any{"input": inputVars})
	if err != nil {
		return EnterpriseOrgResult{}, fmt.Errorf("creating enterprise organisation %q: %w", input.Login, err)
	}

	var result struct {
		CreateEnterpriseOrganization struct {
			Organization struct {
				ID    string `json:"id"`
				Login string `json:"login"`
			} `json:"organization"`
		} `json:"createEnterpriseOrganization"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return EnterpriseOrgResult{}, fmt.Errorf("decoding create org response: %w", err)
	}

	org := result.CreateEnterpriseOrganization.Organization
	return EnterpriseOrgResult{NodeID: org.ID, Login: org.Login}, nil
}
