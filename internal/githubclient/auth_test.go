// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package githubclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testRSAKey is a 2048-bit RSA private key used only in tests.
// It must never be used with real GitHub credentials.
const testRSAKey = `-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEAqIeh0/zG7fIrUh1175KqT45DY8xGlwPbReTIk8IcHeQwA4QR
UyjKwgH4agllxET2jLxli0GJP1GF9C46eQUADKyRCkBps7MTRD72kLuIMGZDy7XP
9n/G0/OIwDwbpIEfmE0qwPeVsvhPhetulOqdqd0cM9qo3+yi3RiwNwu7YX6y2Vl8
kTJU57jm0kj3g22XU8I/Km0RUmzyZ8nqjhjy6ziNgMvCg9pqPGQl+HdNCK6Dr9Aj
SbWuU40pT6CY6A8kEfD1PBdbO37Y9CiGTAGtsjmglvtch+C4yI5Y4AMSrpPWTWdF
gGy7NXQZtdJLczOYcixUb/coevJEl6k6J0WAVQIDAQABAoIBAAWPwkqnYR45m23g
jIONNMMe2AD7XQn/RdVy5R6hLYHcBaycB2FV64kG/R5stHfLadJ/ph84GLIm+9Nw
0hury1JfwIKU2RglzPk31bW1hptISKz4kUYadeKJOyZS5Xuiijshsss+8GkBUkiC
WjoeWvkf054vdVq8aax/s9MBN1wSf06+BgCoqaiWMOKosNzTKINDbdKTRhX5VobC
SfoBbXeyx1xBAyi+/+7yPpGtFjG1RS/rOTyXz7P4hu4UENTB2Lz05lhORECieFzl
6Mt6cc++TCu18dHCM2czP/ww/2h4LqsgE1y/Hko8nCkjMX/2+kXye7J2q9ig/ULG
aAlZ6+kCgYEA4IeRuqI00Kgtt+gkmhVhqSrnoNfWOhGDMq8ziajZo3e0DOkNkvz/
mXNhTEDeSev2AVU/V85PHrrDXUT8uroPm1o8CaQkXrC7wiy2uMtGZUsl4BOSC/Ce
+UGM0OM8npCJ1d+pGGtQxjmZUyJkvSJfUQV8ou62PGeBLhIWSf+rx+kCgYEAwCa2
5AgQkNq94KlixZd5Dhk8f5sMa5GW+uxOcTIFKlP5FnFJ1yJ1xHOlmrmuJ/p8r8jd
ibiILAQe4rniWOONqi4HU+kTvObEReAfISwwSOIqsikGHne1nHFqUu+b2+Z0ZeSG
kwSoi0TXFr2ksvYh7gGuWDt2NdArSyoGuWELHY0CgYEAkSORdE9+TJMqWoNZhbDk
nHH7oOFkvcysPos6iXX4mc67OM091RJuN0d6Ucxs5OP+9gWhGKVoR7j6qMP7isjT
Zd0Cikjsqbkc5fv5caMVMk1NgnekJMu6N+3DlRQPD4DnWLVnnT1hzYFWN4M4E3qw
mrMtSjV837cYritK9TKsXGECgYAiKD+mtZBMT7YlM7ctLMoGKZJJlMRWcuEF5e/j
y2KDrb2/sY/QwH1y2KP9pzhAPxTfIrPPAZCjUnAzGZwU9Q5/zALddbdegx8s1LRz
7yj+K8YvOX+u9tS/5KFj8Ngh9QuH+WG6zL8xUqFxl3Cpp3tMldvqL1fKJSEtEWF0
nr2dGQKBgFwxjBM5T3PqXYokvaGHuvA9NX93DhyZ+8h+nXpvLugcgrf9ucacCBOD
bRhDp09j6xbh/2/XP82po2hkswBsiteHzhfIzut8wb5uwD6n0x6ZFM6rsRqIUfUG
SguHoOJvxEsGvasAqFvQwrmTdu13IYwY19fRGfc7zkaxd+3+bUP4
-----END RSA PRIVATE KEY-----`

func TestGenerateJWT_Claims(t *testing.T) {
	tokenStr, err := generateJWT("app-123", []byte(testRSAKey))
	if err != nil {
		t.Fatalf("generateJWT returned error: %v", err)
	}

	// Parse without verification to inspect claims.
	parsed, _, err := jwt.NewParser().ParseUnverified(tokenStr, jwt.MapClaims{})
	if err != nil {
		t.Fatalf("parsing generated JWT: %v", err)
	}

	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatal("unexpected claims type")
	}

	// issuer must match appID
	iss, err := claims.GetIssuer()
	if err != nil || iss != "app-123" {
		t.Errorf("expected iss=app-123, got %q (err=%v)", iss, err)
	}

	// iat must be in the past (clock-skew back-date)
	iatVal, _ := claims["iat"].(float64)
	iat := time.Unix(int64(iatVal), 0)
	if !iat.Before(time.Now()) {
		t.Errorf("expected iat to be in the past, got %v", iat)
	}

	// iat must not be more than 70 seconds ago (60s back-date + margin)
	if time.Since(iat) > 70*time.Second {
		t.Errorf("iat is too far in the past: %v ago", time.Since(iat))
	}

	// exp must be in the future
	expVal, _ := claims["exp"].(float64)
	exp := time.Unix(int64(expVal), 0)
	if !exp.After(time.Now()) {
		t.Errorf("expected exp to be in the future, got %v", exp)
	}

	// exp must be within ~9 minutes from now
	if time.Until(exp) > 10*time.Minute {
		t.Errorf("exp is more than 10 minutes from now: %v", time.Until(exp))
	}
}

func TestGenerateJWT_InvalidKey(t *testing.T) {
	_, err := generateJWT("app-123", []byte("not a pem key"))
	if err == nil {
		t.Fatal("expected error for invalid PEM, got nil")
	}
}

func TestTokenCache_Hit(t *testing.T) {
	cache := NewTokenCache()
	ctx := context.Background()

	var fetchCount atomic.Int32
	futureExpiry := time.Now().Add(30 * time.Minute)

	fetch := func() (string, time.Time, error) {
		fetchCount.Add(1)
		return "token-abc", futureExpiry, nil
	}

	tok1, err := cache.Get(ctx, "install-1", fetch)
	if err != nil {
		t.Fatalf("first Get error: %v", err)
	}
	if tok1 != "token-abc" {
		t.Errorf("expected token-abc, got %q", tok1)
	}

	tok2, err := cache.Get(ctx, "install-1", fetch)
	if err != nil {
		t.Fatalf("second Get error: %v", err)
	}
	if tok2 != "token-abc" {
		t.Errorf("expected token-abc, got %q", tok2)
	}

	if fetchCount.Load() != 1 {
		t.Errorf("expected fetch to be called once (cache hit), called %d times", fetchCount.Load())
	}
}

func TestTokenCache_Miss_OnExpiredSoon(t *testing.T) {
	cache := NewTokenCache()
	ctx := context.Background()

	var fetchCount atomic.Int32

	// Pre-populate the cache with a token that expires in 2 minutes (below the 5-min margin).
	cache.tokens["install-2"] = cachedToken{
		token:     "old-token",
		expiresAt: time.Now().Add(2 * time.Minute),
	}

	newExpiry := time.Now().Add(60 * time.Minute)
	fetch := func() (string, time.Time, error) {
		fetchCount.Add(1)
		return "new-token", newExpiry, nil
	}

	tok, err := cache.Get(ctx, "install-2", fetch)
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if tok != "new-token" {
		t.Errorf("expected new-token, got %q", tok)
	}
	if fetchCount.Load() != 1 {
		t.Errorf("expected fetch to be called once (cache miss), called %d times", fetchCount.Load())
	}
}

func TestTokenCache_FetchError(t *testing.T) {
	cache := NewTokenCache()
	ctx := context.Background()

	fetch := func() (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("auth failed")
	}

	_, err := cache.Get(ctx, "install-3", fetch)
	if err == nil {
		t.Fatal("expected error from fetch, got nil")
	}
}

func TestGetInstallationToken(t *testing.T) {
	expiry := time.Now().Add(60 * time.Minute).UTC().Truncate(time.Second)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-jwt" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "ghs_test_token",
			"expires_at": expiry.Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	tok, exp, err := getInstallationToken(context.Background(), srv.URL, "99", "test-jwt")
	if err != nil {
		t.Fatalf("getInstallationToken error: %v", err)
	}
	if tok != "ghs_test_token" {
		t.Errorf("expected ghs_test_token, got %q", tok)
	}
	if !exp.Equal(expiry) {
		t.Errorf("expected expiry %v, got %v", expiry, exp)
	}
}

func TestGetInstallationToken_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	_, _, err := getInstallationToken(context.Background(), srv.URL, "99", "jwt")
	if err == nil {
		t.Fatal("expected error for non-201 status, got nil")
	}
}
