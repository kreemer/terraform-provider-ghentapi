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
//
// GET /app/installations/{installation_id} must be authenticated as the app
// itself via a JWT — it explicitly rejects installation access tokens — so
// this uses DoWithEnterpriseAppJWT rather than DoWithEnterpriseAuth.
func (c *Client) resolveEnterpriseSlug(ctx context.Context) (string, error) {
	c.enterpriseSlugOnce.Do(func() {
		path := fmt.Sprintf("/app/installations/%s", c.cfg.EnterpriseAppInstallationID)
		resp, err := c.DoWithEnterpriseAppJWT(ctx, http.MethodGet, path, nil)
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
				Slug string `json:"slug"`
			} `json:"account"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			c.enterpriseSlugErr = fmt.Errorf("decoding enterprise installation response: %w", err)
			return
		}
		if result.Account.Slug == "" {
			c.enterpriseSlugErr = fmt.Errorf("enterprise installation response missing account.slug")
			return
		}
		c.enterpriseSlug = result.Account.Slug
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
	ClientID            string `json:"client_id"`
	RepositorySelection string `json:"repository_selection"`
}

// listOrgInstallations fetches all GitHub App installations for the given
// organisation via the enterprise API. Returns an empty slice (not an error)
// when the API responds with 404.
func (c *Client) listOrgInstallations(ctx context.Context, enterpriseSlug, org string) ([]orgInstallation, error) {
	path := fmt.Sprintf("/enterprises/%s/apps/organizations/%s/installations", enterpriseSlug, org)
	resp, err := c.DoWithEnterpriseAuth(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("listing org installations: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading org installations response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing org installations failed (status %d): %s", resp.StatusCode, string(body))
	}

	var installations []orgInstallation
	if err := json.Unmarshal(body, &installations); err != nil {
		// Some API versions wrap the list; try the wrapper form.
		var wrapper struct {
			Installations []orgInstallation `json:"installations"`
		}
		if err2 := json.Unmarshal(body, &wrapper); err2 != nil {
			return nil, fmt.Errorf("decoding org installations: %w", err)
		}
		installations = wrapper.Installations
	}
	return installations, nil
}

// findOrgInstallation searches the enterprise API for an existing installation
// of our org app in the given organisation. Returns "" if not found.
func (c *Client) findOrgInstallation(ctx context.Context, enterpriseSlug, org string) (string, error) {
	installations, err := c.listOrgInstallations(ctx, enterpriseSlug, org)
	if err != nil {
		return "", err
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
	installation, err := c.installApp(ctx, enterpriseSlug, org, c.cfg.OrgAppClientID, c.cfg.RepositorySelection, nil)
	if err != nil {
		return "", err
	}
	return installation.ID, nil
}

// AppInstallation describes a GitHub App installation on an organisation.
type AppInstallation struct {
	ID                  string
	AppSlug             string
	ClientID            string
	RepositorySelection string
}

// InstallGitHubApp installs the GitHub App identified by clientID into org,
// authenticating with the enterprise app token. repositorySelection must be
// "all", "selected", or "none". repositories is only sent (and only
// meaningful) when repositorySelection is "selected". Calling this again for
// an already-installed app updates its repository selection, per GitHub's
// API semantics.
func (c *Client) InstallGitHubApp(ctx context.Context, org, clientID, repositorySelection string, repositories []string) (AppInstallation, error) {
	slug, err := c.resolveEnterpriseSlug(ctx)
	if err != nil {
		return AppInstallation{}, err
	}
	return c.installApp(ctx, slug, org, clientID, repositorySelection, repositories)
}

// installApp performs the actual install/update API call.
func (c *Client) installApp(ctx context.Context, enterpriseSlug, org, clientID, repositorySelection string, repositories []string) (AppInstallation, error) {
	path := fmt.Sprintf("/enterprises/%s/apps/organizations/%s/installations", enterpriseSlug, org)
	payload := map[string]interface{}{
		"client_id":            clientID,
		"repository_selection": repositorySelection,
	}
	if len(repositories) > 0 {
		payload["repositories"] = repositories
	}
	resp, err := c.DoWithEnterpriseAuth(ctx, http.MethodPost, path, payload)
	if err != nil {
		return AppInstallation{}, fmt.Errorf("installing github app: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return AppInstallation{}, fmt.Errorf("reading install response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return AppInstallation{}, fmt.Errorf("installing github app failed (status %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		ID                  int64  `json:"id"`
		AppSlug             string `json:"app_slug"`
		ClientID            string `json:"client_id"`
		RepositorySelection string `json:"repository_selection"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return AppInstallation{}, fmt.Errorf("decoding install response: %w", err)
	}
	return AppInstallation{
		ID:                  fmt.Sprintf("%d", result.ID),
		AppSlug:             result.AppSlug,
		ClientID:            result.ClientID,
		RepositorySelection: result.RepositorySelection,
	}, nil
}

// FindGitHubAppInstallation searches the given organisation for an existing
// installation of the app identified by clientID. ok is false when no such
// installation exists.
func (c *Client) FindGitHubAppInstallation(ctx context.Context, org, clientID string) (installation AppInstallation, ok bool, err error) {
	slug, err := c.resolveEnterpriseSlug(ctx)
	if err != nil {
		return AppInstallation{}, false, err
	}
	installations, err := c.listOrgInstallations(ctx, slug, org)
	if err != nil {
		return AppInstallation{}, false, err
	}
	for _, inst := range installations {
		if inst.ClientID == clientID {
			return AppInstallation{
				ID:                  fmt.Sprintf("%d", inst.ID),
				AppSlug:             inst.AppSlug,
				ClientID:            inst.ClientID,
				RepositorySelection: inst.RepositorySelection,
			}, true, nil
		}
	}
	return AppInstallation{}, false, nil
}

// UninstallGitHubApp removes the given installation from the organisation. A
// 404 response is treated as success, since the installation is already gone.
func (c *Client) UninstallGitHubApp(ctx context.Context, org, installationID string) error {
	slug, err := c.resolveEnterpriseSlug(ctx)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/enterprises/%s/apps/organizations/%s/installations/%s", slug, org, installationID)
	resp, err := c.DoWithEnterpriseAuth(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return fmt.Errorf("uninstalling github app: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading uninstall response: %w", err)
	}
	return fmt.Errorf("uninstalling github app failed (status %d): %s", resp.StatusCode, string(body))
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

// DoWithEnterpriseAppJWT executes a request authenticated with the enterprise
// app's own JWT (signed with its private key), rather than an installation
// access token. Some endpoints — such as "Get an installation for the
// authenticated app" — explicitly require app-level JWT authentication and
// reject installation access tokens.
func (c *Client) DoWithEnterpriseAppJWT(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	appJWT, err := generateJWT(c.cfg.EnterpriseAppID, c.cfg.EnterpriseAppPEM)
	if err != nil {
		return nil, fmt.Errorf("generating enterprise app JWT: %w", err)
	}
	return c.Do(ctx, method, path, body, map[string]string{
		"Authorization": "Bearer " + appJWT,
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
	return c.doGraphQLWithToken(ctx, tok, query, variables)
}

// DoGraphQLWithOrgAuth executes a GraphQL query or mutation authenticated as
// the org-level GitHub App installed in the given organisation. The org app
// installation is resolved/installed automatically via EnsureOrgInstallation.
func (c *Client) DoGraphQLWithOrgAuth(ctx context.Context, org, query string, variables map[string]any) (json.RawMessage, error) {
	tok, err := c.OrgToken(ctx, org)
	if err != nil {
		return nil, fmt.Errorf("obtaining org token for %q for GraphQL: %w", org, err)
	}
	return c.doGraphQLWithToken(ctx, tok, query, variables)
}

// doGraphQLWithToken executes a GraphQL query or mutation using the given
// bearer token. Retries are performed for HTTP 429 and 5xx responses (same
// policy as Do).
func (c *Client) doGraphQLWithToken(ctx context.Context, tok string, query string, variables map[string]any) (json.RawMessage, error) {
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
	// profileName is required by the createEnterpriseOrganization mutation;
	// default it to the org login when no display name was provided.
	profileName := input.DisplayName
	if profileName == "" {
		profileName = input.Login
	}
	inputVars["profileName"] = profileName

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

// CostCenterResource describes a single resource (user, organization,
// repository, or enterprise team) assigned to a cost center.
type CostCenterResource struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// CostCenterAICreditPoolState describes the AI credit pool cap for a cost
// center, when ai_credit_pool_enabled is true.
type CostCenterAICreditPoolState struct {
	TargetAmount  *float64 `json:"target_amount"`
	CurrentAmount *float64 `json:"current_amount"`
}

// CostCenter is the enterprise billing cost center resource.
type CostCenter struct {
	ID                  string
	Name                string
	State               string
	AzureSubscription   *string
	AICreditPoolEnabled bool
	AICreditPoolState   *CostCenterAICreditPoolState
	Resources           []CostCenterResource
}

// costCenterAPIResponse mirrors the JSON shape returned by the cost center
// create/get/update endpoints.
type costCenterAPIResponse struct {
	ID                  string                       `json:"id"`
	Name                string                       `json:"name"`
	State               string                       `json:"state"`
	AzureSubscription   *string                      `json:"azure_subscription"`
	AICreditPoolEnabled bool                         `json:"ai_credit_pool_enabled"`
	AICreditPoolState   *CostCenterAICreditPoolState `json:"ai_credit_pool_state"`
	Resources           []CostCenterResource         `json:"resources"`
}

func (r costCenterAPIResponse) toCostCenter() CostCenter {
	return CostCenter(r)
}

// CostCenterResourceChanges groups resources by type for the add/remove
// resource endpoints.
type CostCenterResourceChanges struct {
	Users           []string
	Organizations   []string
	Repositories    []string
	EnterpriseTeams []string
}

// isEmpty reports whether all resource slices are empty.
func (c CostCenterResourceChanges) isEmpty() bool {
	return len(c.Users) == 0 && len(c.Organizations) == 0 && len(c.Repositories) == 0 && len(c.EnterpriseTeams) == 0
}

func (c CostCenterResourceChanges) toPayload() map[string]interface{} {
	payload := map[string]interface{}{}
	if len(c.Users) > 0 {
		payload["users"] = c.Users
	}
	if len(c.Organizations) > 0 {
		payload["organizations"] = c.Organizations
	}
	if len(c.Repositories) > 0 {
		payload["repositories"] = c.Repositories
	}
	if len(c.EnterpriseTeams) > 0 {
		payload["enterprise_teams"] = c.EnterpriseTeams
	}
	return payload
}

// CreateCostCenter creates a new enterprise cost center, authenticated as the
// enterprise app.
func (c *Client) CreateCostCenter(ctx context.Context, name string, aiCreditPoolEnabled bool) (CostCenter, error) {
	slug, err := c.resolveEnterpriseSlug(ctx)
	if err != nil {
		return CostCenter{}, err
	}
	path := fmt.Sprintf("/enterprises/%s/settings/billing/cost-centers", slug)
	payload := map[string]interface{}{
		"name":                   name,
		"ai_credit_pool_enabled": aiCreditPoolEnabled,
	}
	resp, err := c.DoWithEnterpriseAuth(ctx, http.MethodPost, path, payload)
	if err != nil {
		return CostCenter{}, fmt.Errorf("creating cost center: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return CostCenter{}, fmt.Errorf("reading create cost center response: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return CostCenter{}, fmt.Errorf("creating cost center failed (status %d): %s", resp.StatusCode, string(body))
	}

	var result costCenterAPIResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return CostCenter{}, fmt.Errorf("decoding create cost center response: %w", err)
	}
	return result.toCostCenter(), nil
}

// GetCostCenter fetches a cost center by ID. ok is false when the cost center
// does not exist (HTTP 404).
func (c *Client) GetCostCenter(ctx context.Context, costCenterID string) (costCenter CostCenter, ok bool, err error) {
	slug, err := c.resolveEnterpriseSlug(ctx)
	if err != nil {
		return CostCenter{}, false, err
	}
	path := fmt.Sprintf("/enterprises/%s/settings/billing/cost-centers/%s", slug, costCenterID)
	resp, err := c.DoWithEnterpriseAuth(ctx, http.MethodGet, path, nil)
	if err != nil {
		return CostCenter{}, false, fmt.Errorf("getting cost center: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return CostCenter{}, false, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return CostCenter{}, false, fmt.Errorf("reading get cost center response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return CostCenter{}, false, fmt.Errorf("getting cost center failed (status %d): %s", resp.StatusCode, string(body))
	}

	var result costCenterAPIResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return CostCenter{}, false, fmt.Errorf("decoding get cost center response: %w", err)
	}
	return result.toCostCenter(), true, nil
}

// UpdateCostCenter updates the name and/or AI credit pool setting of an
// existing cost center. Pass nil for fields that should not be changed.
func (c *Client) UpdateCostCenter(ctx context.Context, costCenterID string, name *string, aiCreditPoolEnabled *bool) (CostCenter, error) {
	slug, err := c.resolveEnterpriseSlug(ctx)
	if err != nil {
		return CostCenter{}, err
	}
	payload := map[string]interface{}{}
	if name != nil {
		payload["name"] = *name
	}
	if aiCreditPoolEnabled != nil {
		payload["ai_credit_pool_enabled"] = *aiCreditPoolEnabled
	}

	path := fmt.Sprintf("/enterprises/%s/settings/billing/cost-centers/%s", slug, costCenterID)
	resp, err := c.DoWithEnterpriseAuth(ctx, http.MethodPatch, path, payload)
	if err != nil {
		return CostCenter{}, fmt.Errorf("updating cost center: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return CostCenter{}, fmt.Errorf("reading update cost center response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return CostCenter{}, fmt.Errorf("updating cost center failed (status %d): %s", resp.StatusCode, string(body))
	}

	var result costCenterAPIResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return CostCenter{}, fmt.Errorf("decoding update cost center response: %w", err)
	}
	return result.toCostCenter(), nil
}

// DeleteCostCenter archives a cost center. GitHub provides no way to
// permanently delete a cost center; this sets its state to "deleted". A 404
// response is treated as success, since the cost center is already gone.
func (c *Client) DeleteCostCenter(ctx context.Context, costCenterID string) error {
	slug, err := c.resolveEnterpriseSlug(ctx)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/enterprises/%s/settings/billing/cost-centers/%s", slug, costCenterID)
	resp, err := c.DoWithEnterpriseAuth(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return fmt.Errorf("deleting cost center: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading delete cost center response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deleting cost center failed (status %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// AddCostCenterResources assigns users, organizations, repositories, and/or
// enterprise teams to a cost center. Empty changes are a no-op.
func (c *Client) AddCostCenterResources(ctx context.Context, costCenterID string, changes CostCenterResourceChanges) error {
	if changes.isEmpty() {
		return nil
	}
	slug, err := c.resolveEnterpriseSlug(ctx)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/enterprises/%s/settings/billing/cost-centers/%s/resource", slug, costCenterID)
	resp, err := c.DoWithEnterpriseAuth(ctx, http.MethodPost, path, changes.toPayload())
	if err != nil {
		return fmt.Errorf("adding cost center resources: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("adding cost center resources failed (status %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// RemoveCostCenterResources unassigns users, organizations, repositories,
// and/or enterprise teams from a cost center. Empty changes are a no-op.
func (c *Client) RemoveCostCenterResources(ctx context.Context, costCenterID string, changes CostCenterResourceChanges) error {
	if changes.isEmpty() {
		return nil
	}
	slug, err := c.resolveEnterpriseSlug(ctx)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/enterprises/%s/settings/billing/cost-centers/%s/resource", slug, costCenterID)
	resp, err := c.DoWithEnterpriseAuth(ctx, http.MethodDelete, path, changes.toPayload())
	if err != nil {
		return fmt.Errorf("removing cost center resources: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("removing cost center resources failed (status %d): %s", resp.StatusCode, string(body))
	}
	return nil
}
