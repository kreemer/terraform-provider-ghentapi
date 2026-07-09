# Implementation TODO — terraform-provider-ghentapi

> This file tracks all implementation tasks derived from the spec in
> `spec.instructions.md`. Work through each section in order; later sections
> depend on earlier ones.

---

## 1. Repository / Module Bootstrap

- [x] Rename the Go module from `github.com/hashicorp/terraform-provider-scaffolding-framework`
      to `github.com/kreemer/terraform-provider-ghentapi` in `go.mod` and all
      import paths across the codebase.
- [x] Update `main.go`:
  - Change the provider address to `registry.terraform.io/kreemer/ghentapi`.
  - Import `github.com/kreemer/terraform-provider-ghentapi/internal/provider`.
- [x] Remove all scaffolding example files that will not be reused:
  - `internal/provider/example_action.go` + test
  - `internal/provider/example_data_source.go` + test
  - `internal/provider/example_ephemeral_resource.go` + test
  - `internal/provider/example_function.go` + test
  - `internal/provider/example_resource.go` + test
- [x] Add required new dependencies to `go.mod`:
  - `github.com/golang-jwt/jwt/v5` (JWT generation)
  - Verify `github.com/hashicorp/terraform-plugin-framework >= v1.13` is present
    (already at v1.19 — no action needed).
- [x] Run `go mod tidy` to clean up indirect dependencies.

---

## 2. `internal/githubclient/auth.go` — JWT & Token Cache

- [x] Create directory `internal/githubclient/`.
- [x] Implement `generateJWT(appID string, pemKey []byte) (string, error)`:
  - Parse RSA private key from PEM using `jwt.ParseRSAPrivateKeyFromPEM`.
  - Build `MapClaims` with `iat` = now − 60 s, `exp` = now + 9 min, `iss` = appID.
  - Sign with RS256 and return the token string.
- [x] Implement `cachedToken` struct with fields `token string` and
      `expiresAt time.Time`.
- [x] Implement `TokenCache` struct with `mu sync.Mutex` and
      `tokens map[string]cachedToken`.
- [x] Implement `TokenCache.Get(ctx, installationID, fetch func() (string, time.Time, error)) (string, error)`:
  - Return cached token if `time.Until(expiresAt) > 5 * time.Minute`.
  - Otherwise call `fetch()`, store result, and return new token.
- [x] Implement `getInstallationToken(ctx, baseURL, installationID, jwt string) (string, time.Time, error)`:
  - POST `{baseURL}/app/installations/{installationID}/access_tokens` with
    `Authorization: Bearer {jwt}`.
  - Parse `token` and `expires_at` from JSON response.
  - Return token string and expiry time.
- [x] Write unit tests in `auth_test.go`:
  - Test JWT claims (issued-at skew, expiry window, issuer).
  - Test cache hit (fetch not called on second Get within window).
  - Test cache miss / expiry (fetch called when token expires within 5 min).

---

## 3. `internal/githubclient/client.go` — HTTP Client Wrapper

- [x] Implement `Client` struct holding:
  - `baseURL string`
  - `httpClient *http.Client`
  - `enterpriseAppID`, `enterpriseAppInstallationID`, `enterpriseAppPEM []byte`
  - `orgAppID`, `orgAppPEM []byte`
  - `cache *TokenCache`
- [x] Implement `NewClient(baseURL string, ...) *Client` constructor.
- [x] Implement `Client.enterpriseToken(ctx) (string, error)`:
  - Calls `generateJWT` with enterprise app credentials.
  - Calls `cache.Get` with `enterpriseAppInstallationID` and a fetch function
    that calls `getInstallationToken`.
- [x] Implement `Client.orgToken(ctx, installationID string) (string, error)`:
  - Calls `generateJWT` with org app credentials.
  - Calls `cache.Get` with the given `installationID`.
- [x] Implement `Client.Do(ctx, method, path string, body interface{}) (*http.Response, error)`:
  - Serialises body to JSON when non-nil.
  - Executes the HTTP request.
  - On HTTP 429 or 5xx, retries up to 3 times with exponential back-off
    (1 s, 2 s, 4 s).
  - Returns the final response or an error.
- [x] Implement `Client.DoWithEnterpriseAuth(ctx, method, path string, body interface{}) (*http.Response, error)`:
  - Fetches enterprise token and injects `Authorization: token {tok}` header
    before calling `Do`.
- [x] Implement `Client.DoWithOrgAuth(ctx, installationID, method, path string, body interface{}) (*http.Response, error)`:
  - Fetches org token for `installationID` and injects header before calling `Do`.

---

## 4. `internal/provider/provider.go` — Provider Definition

- [x] Rename `ScaffoldingProvider` → `GhentapiProvider` and update all
      references.
- [x] Update `Metadata` to set `TypeName = "ghentapi"`.
- [x] Define `GhentapiProviderModel` with all required attributes (all
      `types.String`):
  - `base_url` (optional, default `"https://api.github.com"`)
  - `enterprise_app_id` (required)
  - `enterprise_app_installation_id` (required)
  - `enterprise_app_pem_file` (required, sensitive)
  - `org_app_id` (required)
  - `org_app_client_id` (optional — used in resource schema)
  - `org_app_pem_file` (required, sensitive)
- [x] Implement `Schema` with appropriate `MarkdownDescription` and sensitivity
      markers for PEM fields.
- [x] Implement `Configure`:
  - Validate that required fields are not null/unknown.
  - Instantiate `githubclient.NewClient(...)` using the configured values.
  - Set `resp.DataSourceData` and `resp.ResourceData` to the client.
- [x] Register resources: `NewOrgAppInstallationResource`, `NewOrgSettingResource`.
- [x] Register data sources: `NewInstallationTokenDataSource`.
- [x] Remove registrations of all scaffolding example resources / data sources /
      functions / actions.
- [x] Drop `provider.ProviderWithFunctions`, `ProviderWithEphemeralResources`,
      and `ProviderWithActions` interface assertions if those capabilities are
      not used.

---

## 5. REMOVED — replaced by implicit installation

> Design change: The explicit resource was replaced by `EnsureOrgInstallation`
> in the provider client. See section 3 (client.go) for implementation.

- [x] Removed `resource_org_app_installation.go`
- [x] Provider config: added `auto_install_org_app`, `repository_selection`, made `org_app_client_id` required
- [x] Added `ClientConfig` struct and refactored `NewClient`
- [x] Implemented `Client.EnsureOrgInstallation(ctx, org)`: cache → list → install/error
- [x] Implemented `Client.OrgToken(ctx, org)` via `EnsureOrgInstallation`
- [x] `DoWithOrgAuth` now takes `org` instead of `installationID`
- [x] 8 client tests pass

---

## 6. `internal/provider/resource_org_setting.go`

- [x] Define `OrgSettingResource` struct implementing `resource.Resource`.
- [x] Define `OrgSettingModel` with tfsdk tags:
  - `organization` (string, required, forces-new)
  - `settings` (map of string, required)
  - `api_response` (string, computed, sensitive — full JSON for debugging)
- [x] Implement `Metadata` → type name `"ghentapi_org_setting"`.
- [x] Implement `Schema`.
- [x] Implement `Create` / `Update` (same logic):
  - PATCH `/orgs/{org}` with the `settings` map as JSON body using
    `client.DoWithOrgAuth(ctx, org, ...)` (installation resolved automatically).
  - Store full response JSON in `api_response`.
- [x] Implement `Read`:
  - GET `/orgs/{org}` using org token.
  - Unmarshal full response JSON.
  - For each key in `state.Settings`, extract the matching value from the API
    response and update `state.Settings[key]`.
  - Ignore all keys not present in the current settings map (no drift on
    unmanaged fields).
  - Store full response in `api_response`.
- [x] Implement `Delete` → no-op (just remove from state; log a debug message).
- [x] Write acceptance tests:
  - Create + Read lifecycle with mock org API.
  - Update (PATCH) when a settings value changes.
  - Drift detection: API returns different value → plan shows diff.
  - Extra API fields are ignored.

---

## 7. `internal/provider/datasource_installation_token.go`

- [x] Define `InstallationTokenDataSource` struct implementing
      `datasource.DataSource`.
- [x] Define `InstallationTokenModel` with tfsdk tags:
  - `organization` (string, required)
  - `token` (string, computed, sensitive)
- [x] Implement `Metadata` → type name `"ghentapi_installation_token"`.
- [x] Implement `Schema` with `token` marked `Sensitive: true`.
- [x] Implement `Read`:
  - Call `client.OrgToken(ctx, org)` to get a fresh token via `EnsureOrgInstallation`.
  - Set `state.Token` — token is never stored in permanent state because
    `sensitive` data sources are re-evaluated on every plan/apply.
  - **Do not** persist the token value anywhere else.
- [x] Write a unit test verifying that the token attribute is marked sensitive
      in the schema.

---

## 8. Examples & Documentation

- [x] Replace `examples/provider/provider.tf` with a real `ghentapi` provider
      example (matching the spec's "Example full configuration").
- [x] Add `examples/resources/ghentapi_org_setting/resource.tf`.
- [x] Add `examples/data-sources/ghentapi_installation_token/data-source.tf`.
- [x] Remove scaffolding example files from `examples/` (actions, ephemeral-resources, scaffolding dirs).
- [x] Update `docs/index.md` with the provider overview and configuration
      reference.
- [x] Update `README.md` to describe the provider purpose, requirements, and
      usage.

---

## 9. CI / Tooling

- [x] Update `.goreleaser.yml` — no changes needed (uses `{{ .ProjectName }}` template).
- [x] Verify `GNUmakefile` targets (`build`, `test`, `testacc`, `lint`) work
      with the new module path.
- [x] `go vet ./...` passes with no errors.
- [x] `golangci-lint run` passes with 0 issues.
- [x] GitHub Actions test workflow (`.github/workflows/test.yml`) runs unit tests
      and acceptance tests (using `resource.UnitTest` — no real GitHub credentials required).

---

## Dependency Order

```
1 (module rename)
  └─> 2 (auth.go)
        └─> 3 (client.go)
              └─> 4 (provider.go)
                    ├─> 5 (resource_org_app_installation.go)
                    ├─> 6 (resource_org_setting.go)
                    └─> 7 (datasource_installation_token.go)
                          └─> 8 (examples + docs)
                                └─> 9 (CI / tooling)
```

---

## Key Constraints (must be respected throughout)

- **No token in state** — provider, resource, or data source state must never
  contain an installation token or JWT.
- **In-memory cache** — `TokenCache` is the single shared cache for the entire
  provider lifetime; share via provider `Configure`.
- **GHES + GHEC** — `base_url` selects the endpoint; no compile-time switch.
- **Retries** — HTTP 429 and 5xx: up to 3 retries with exponential back-off.
- **Terraform Plugin Framework** — use `hashicorp/terraform-plugin-framework`,
  not the legacy SDK.
- **Go module path** — `github.com/kreemer/terraform-provider-ghentapi`.
