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
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient returns a Client pointed at baseURL with the test RSA key
// pre-loaded for both enterprise and org app credentials.
func newTestClient(baseURL string) *Client {
	return NewClient(
		baseURL,
		"ent-app-id",
		"ent-install-id",
		[]byte(testRSAKey),
		"org-app-id",
		[]byte(testRSAKey),
	)
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
	// Serve two endpoints: the token endpoint and the real endpoint.
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

func TestClient_DoWithOrgAuth_InjectsHeader(t *testing.T) {
	var authHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/org-install-42/access_tokens" {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":      "org-token-abc",
				"expires_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
			})
			return
		}
		authHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	resp, err := c.DoWithOrgAuth(context.Background(), "org-install-42", http.MethodGet, "/orgs/test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if authHeader != "token org-token-abc" {
		t.Errorf("expected Authorization: token org-token-abc, got %q", authHeader)
	}
}
