// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package githubclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// generateJWT creates a signed RS256 JWT for authenticating as a GitHub App.
// iat is back-dated by 60 seconds to tolerate clock skew between client and GitHub.
func generateJWT(appID string, pemKey []byte) (string, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM(pemKey)
	if err != nil {
		return "", fmt.Errorf("parsing RSA private key: %w", err)
	}

	now := time.Now()
	claims := jwt.MapClaims{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": appID,
	}

	token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		return "", fmt.Errorf("signing JWT: %w", err)
	}

	return token, nil
}

// cachedToken holds an installation access token together with its expiry time.
type cachedToken struct {
	token     string
	expiresAt time.Time
}

// TokenCache is a thread-safe in-memory store for GitHub App installation tokens.
// Tokens are reused until 5 minutes before their expiry to avoid using a token
// that expires mid-request.
type TokenCache struct {
	mu     sync.Mutex
	tokens map[string]cachedToken
}

// NewTokenCache returns an initialised TokenCache.
func NewTokenCache() *TokenCache {
	return &TokenCache{
		tokens: make(map[string]cachedToken),
	}
}

// Get returns a valid installation token for installationID.
// If a cached token has more than 5 minutes remaining it is returned directly.
// Otherwise fetch is called to obtain a new token, which is then cached and returned.
func (c *TokenCache) Get(ctx context.Context, installationID string, fetch func() (string, time.Time, error)) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if t, ok := c.tokens[installationID]; ok && time.Until(t.expiresAt) > 5*time.Minute {
		return t.token, nil
	}

	tok, exp, err := fetch()
	if err != nil {
		return "", err
	}

	c.tokens[installationID] = cachedToken{token: tok, expiresAt: exp}
	return tok, nil
}

// installationTokenResponse is the subset of the GitHub API response we care about.
type installationTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// getInstallationToken exchanges a GitHub App JWT for a short-lived installation
// access token by calling POST /app/installations/{installationID}/access_tokens.
func getInstallationToken(ctx context.Context, baseURL, installationID, appJWT string) (string, time.Time, error) {
	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", baseURL, installationID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("building installation token request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("requesting installation token: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("reading installation token response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, fmt.Errorf("installation token request failed (status %d): %s", resp.StatusCode, string(body))
	}

	var result installationTokenResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", time.Time{}, fmt.Errorf("decoding installation token response: %w", err)
	}

	expiresAt, err := time.Parse(time.RFC3339, result.ExpiresAt)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("parsing token expiry %q: %w", result.ExpiresAt, err)
	}

	return result.Token, expiresAt, nil
}
