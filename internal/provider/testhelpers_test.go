// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

// testRSAPEM is a 2048-bit RSA private key used only in tests.
// Must never be used with real GitHub credentials.
const testRSAPEM = `-----BEGIN RSA PRIVATE KEY-----
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

// orgState holds the mutable settings for the mock org.
type orgState struct {
	BillingEmail string
	Description  string
}

// newMockGitHubServer starts a mock GitHub API server for unit tests.
// The returned state pointer is mutated by PATCH calls.
func newMockGitHubServer(t *testing.T) (*httptest.Server, *orgState) {
	t.Helper()
	state := &orgState{BillingEmail: "initial@example.com", Description: "initial desc"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

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
				"account": map[string]string{"login": "test-enterprise"},
			})

		case "/enterprises/test-enterprise/apps/organizations/my-org/installations":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{"id": 42, "client_id": "org-client-id"},
			})

		case "/app/installations/42/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":      "org-token",
				"expires_at": time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339),
			})

		case "/orgs/my-org":
			if r.Method == http.MethodPatch {
				var patch map[string]string
				_ = json.NewDecoder(r.Body).Decode(&patch)
				if v, ok := patch["billing_email"]; ok {
					state.BillingEmail = v
				}
				if v, ok := patch["description"]; ok {
					state.Description = v
				}
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"login":         "my-org",
				"billing_email": state.BillingEmail,
				"description":   state.Description,
				"extra_field":   "should-be-ignored",
			})

		default:
			http.Error(w, fmt.Sprintf(`{"message":"not found: %s"}`, r.URL.Path), http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, state
}

// providerConfig returns a provider block HCL using the given base URL and test keys.
func providerConfig(baseURL string) string {
	// Escape the PEM for HCL — replace newlines with \n for inline string.
	escapedPEM := strings.ReplaceAll(testRSAPEM, "\n", "\\n")
	return fmt.Sprintf(`
provider "ghentapi" {
  base_url                       = %q
  enterprise_app_id              = "ent-app-id"
  enterprise_app_installation_id = "ent-install-id"
  enterprise_app_pem_file        = "%s"
  org_app_id                     = "org-app-id"
  org_app_client_id              = "org-client-id"
  org_app_pem_file               = "%s"
  auto_install_org_app           = true
}
`, baseURL, escapedPEM, escapedPEM)
}

// unitTestFactories returns provider factories for use with resource.UnitTest.
func unitTestFactories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"ghentapi": providerserver.NewProtocol6WithError(New("test")()),
	}
}
