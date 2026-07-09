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
	"time"
)

// Client is an HTTP client that authenticates as a GitHub App and handles
// token lifecycle automatically. Tokens are cached in memory and never written
// to Terraform state.
type Client struct {
	baseURL    string
	httpClient *http.Client

	// Enterprise GitHub App credentials — used to install the org app.
	enterpriseAppID             string
	enterpriseAppInstallationID string
	enterpriseAppPEM            []byte

	// Org-level GitHub App credentials — used to manage org settings.
	orgAppID  string
	orgAppPEM []byte

	cache *TokenCache
}

// NewClient constructs a Client. All PEM values must be raw PEM-encoded RSA
// private keys. baseURL should not have a trailing slash.
func NewClient(
	baseURL string,
	enterpriseAppID string,
	enterpriseAppInstallationID string,
	enterpriseAppPEM []byte,
	orgAppID string,
	orgAppPEM []byte,
) *Client {
	return &Client{
		baseURL:                     baseURL,
		httpClient:                  &http.Client{Timeout: 30 * time.Second},
		enterpriseAppID:             enterpriseAppID,
		enterpriseAppInstallationID: enterpriseAppInstallationID,
		enterpriseAppPEM:            enterpriseAppPEM,
		orgAppID:                    orgAppID,
		orgAppPEM:                   orgAppPEM,
		cache:                       NewTokenCache(),
	}
}

// enterpriseToken returns a valid installation token for the enterprise app.
func (c *Client) enterpriseToken(ctx context.Context) (string, error) {
	return c.cache.Get(ctx, c.enterpriseAppInstallationID, func() (string, time.Time, error) {
		appJWT, err := generateJWT(c.enterpriseAppID, c.enterpriseAppPEM)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("enterprise JWT: %w", err)
		}
		return getInstallationToken(ctx, c.baseURL, c.enterpriseAppInstallationID, appJWT)
	})
}

// orgToken returns a valid installation token for the given org installation.
func (c *Client) orgToken(ctx context.Context, installationID string) (string, error) {
	return c.cache.Get(ctx, installationID, func() (string, time.Time, error) {
		appJWT, err := generateJWT(c.orgAppID, c.orgAppPEM)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("org JWT: %w", err)
		}
		return getInstallationToken(ctx, c.baseURL, installationID, appJWT)
	})
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

		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
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
// token for the given installationID.
func (c *Client) DoWithOrgAuth(ctx context.Context, installationID, method, path string, body interface{}) (*http.Response, error) {
	tok, err := c.orgToken(ctx, installationID)
	if err != nil {
		return nil, fmt.Errorf("obtaining org token: %w", err)
	}
	return c.Do(ctx, method, path, body, map[string]string{
		"Authorization": "token " + tok,
	})
}
