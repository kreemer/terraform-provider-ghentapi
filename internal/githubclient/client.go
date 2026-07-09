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
