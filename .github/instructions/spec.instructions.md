# Specification: terraform-provider-ghentapi

## Goal

Build a new Terraform provider written in Go that manages GitHub organisations
via the GitHub REST API, authenticating natively via a **GitHub App** (app ID +
private key).  The provider must handle token lifecycle automatically so that
short-lived installation tokens (1-hour expiry) never end up in Terraform state
and are always fresh at request time.

The provider needs two GitHub Apps: One which is installed at the enterprise with
the permission to install other GitHub Apps. The other GitHub App is for the
organisation, which handles org-settings permissions. The enterprise GitHub
App installs the org-app within the organisation **automatically** the first time
a resource targets that organisation (implicit installation).

See https://docs.github.com/en/enterprise-cloud@latest/admin/managing-github-apps-for-your-enterprise/automate-installations
for the official documentation about how to install enterprise/org apps.

---

## Background / Why a new provider

The current setup uses `kreemer/terraform-provider-restapi` with per-resource
`headers = { "Authorization" = data.github_app_token.org[each.key].token }`.

The core problem is that Terraform's **refresh phase** calls `Read()` using the
stale state from the previous run.  That state contains the old (now expired)
installation token.  The freshly-evaluated `data.github_app_token` values are
not available during the refresh phase — Terraform resolves data sources only
after all `Read()` calls have finished.  This causes intermittent `401 Bad
credentials` errors on every plan/apply.

A provider that generates installation tokens itself, on demand, solves this
cleanly: no token ever touches state.

---

## Architecture overview

```
terraform-provider-ghentapi
├── internal/
│   ├── githubclient/
│   │   ├── auth.go          # GitHub App JWT + installation token generation + cache
│   │   └── client.go        # HTTP client wrapper (retries, implicit org installation)
│   └── provider/
│       ├── provider.go      # Provider schema + configuration
│       ├── resource_org_setting.go
│       └── datasource_installation_token.go   # optional helper data source
├── main.go
└── go.mod
```

The provider uses the **Terraform Plugin Framework** (not the older SDK):
- `github.com/hashicorp/terraform-plugin-framework` ≥ v1.13
- `github.com/hashicorp/terraform-plugin-log`
- No dependency on the `integrations/github` provider.

---

## Provider configuration

```hcl
provider "ghentapi" {
  # Base URL — supports both GHES and GHEC
  # GHES:  "https://github.example.com/api/v3"
  # GHEC:  "https://api.github.com"
  base_url = "https://github.unibe.ch/api/v3"

  # Enterprise-level GitHub App credentials
  # Used to install the org-level app into new organisations.
  enterprise_app_id              = var.ent_app_id
  enterprise_app_installation_id = var.ent_app_installation_id
  enterprise_app_pem_file        = var.ent_pem   # sensitive, raw PEM string

  # Org-level GitHub App credentials
  # Used to manage settings within each organisation after installation.
  org_app_id        = var.org_app_id
  org_app_client_id = var.org_app_client_id   # required; used for install lookup
  org_app_pem_file  = var.org_pem             # sensitive, raw PEM string

  # Implicit installation behaviour (optional)
  # When true (default), the org app is installed automatically the first time
  # a resource targets an organisation that doesn't have it yet.
  # When false, an error is returned instead.
  auto_install_org_app = true

  # Repository selection used when auto-installing. "all" or "selected".
  repository_selection = "all"
}
```

### Enterprise slug resolution (internal detail)

The provider resolves the enterprise slug automatically at first use by calling
`GET /app/installations/{enterprise_app_installation_id}` with the enterprise
app JWT. The `account.login` field in the response is the slug. The result is
cached in memory for the lifetime of the Terraform run.

### Token generation (internal detail, not stored in state)

For every API call the provider:
1. Signs a JWT using the relevant app's private key (RS256, 10-minute expiry).
2. Calls `POST /app/installations/{installation_id}/access_tokens` to obtain a
   fresh installation token.
3. Caches the token in memory (with its `expires_at` timestamp) for the
   duration of a single Terraform run.  The cache is invalidated 5 minutes
   before expiry to give a safety margin.
4. **Never writes any token to Terraform state.**

### Implicit org app installation (internal detail)

When a resource first targets an organisation, `EnsureOrgInstallation(ctx, org)`
is called:
1. Check the in-memory org→installation_id cache.
2. Call `GET /enterprises/{slug}/apps/organizations/{org}/installations` and
   search for an entry whose `client_id` matches `org_app_client_id`.
3. If found: cache the installation ID and continue.
4. If not found and `auto_install_org_app = true`: call
   `POST /enterprises/{slug}/apps/organizations/{org}/installations` to install,
   cache the new ID, and continue.
5. If not found and `auto_install_org_app = false`: return a descriptive error.

The app is **never uninstalled** when resources are destroyed (safe default).

---

## Resource: `ghentapi_org_setting`

Manages a set of organisation-level settings via `PATCH /orgs/{org}`.
Equivalent to the current `restapi_setting.this`.

```hcl
resource "ghentapi_org_setting" "this" {
  for_each = toset(var.organizations)

  organization = each.key

  # Map of setting key → value.  Only keys present here are managed;
  # all other org settings are left untouched (equivalent to
  # ignore_server_additions = true in restapi_setting).
  settings = {
    billing_email = "github.id@example.ch"
  }
}
```

### Attributes

| Attribute       | Type        | Description                                              |
|----------------|-------------|----------------------------------------------------------|
| `organization` | string      | GitHub organisation login                               |
| `settings`     | map(string) | Keys/values to manage.  Only these are drift-checked.   |
| `api_response` | string      | Full JSON body from the last successful GET (for debug). |

### API mapping

| Terraform lifecycle | HTTP method | Path           |
|--------------------|-------------|----------------|
| Create / Update    | PATCH       | `/orgs/{org}` |
| Read               | GET         | `/orgs/{org}` |
| Delete             | no-op       | (settings can't be "deleted", resource is just removed from state) |

### Authentication

The provider calls `EnsureOrgInstallation(ctx, org)` to obtain the installation
ID, then uses the org app installation token. The token is generated fresh per
API call (cached in memory, never in state).

### Drift detection

After `Read()`, only the keys present in `settings` are compared against the
API response.  Extra fields returned by the API are silently ignored.

---

## Data source: `ghentapi_installation_token` (optional / nice to have)

Exposes the org app installation token as a data source so it can be passed to
other providers if needed.  The token is marked `sensitive = true` and is
**not** stored in state.

```hcl
data "ghentapi_installation_token" "org" {
  for_each     = toset(var.organizations)
  organization = each.key
}
```

### Attributes

| Attribute      | Type   | Description                                   |
|---------------|--------|-----------------------------------------------|
| `organization` | string | GitHub organisation login (input)            |
| `token`        | string | Short-lived installation token (sensitive)   |

---

## Example full configuration

```hcl
terraform {
  required_version = "~> 1.11"
  required_providers {
    ghentapi = {
      source  = "kreemer/ghentapi"
      version = "~> 1.0"
    }
  }
}

provider "ghentapi" {
  base_url                       = "https://github.unibe.ch/api/v3"
  enterprise_app_id              = var.ent_app_id
  enterprise_app_installation_id = var.ent_app_installation_id
  enterprise_app_pem_file        = var.ent_pem
  org_app_id                     = var.org_app_id
  org_app_client_id              = var.org_app_client_id
  org_app_pem_file               = var.org_pem
  auto_install_org_app           = true
  repository_selection           = "all"
}

resource "ghentapi_org_setting" "this" {
  for_each     = toset(var.organizations)
  organization = each.key
  settings = {
    billing_email = "github.id@unibe.ch"
  }
}
```

---

## Non-functional requirements

1. **No token in state** — provider-level and per-resource tokens must never
   appear in any part of `terraform.tfstate`.
2. **In-memory token cache** — cache tokens for their remaining lifetime minus
   a 5-minute safety margin; share the cache across all resources in one run.
3. **In-memory installation cache** — cache org→installation_id mappings for
   the duration of a single Terraform run.
4. **GHES + GHEC** — the `base_url` provider attribute selects the endpoint;
   no compile-time switch.
5. **Retries** — on HTTP 429 or 5xx, retry up to 3 times with exponential
   back-off.
6. **Terraform Plugin Framework** — use `hashicorp/terraform-plugin-framework`,
   not the legacy `hashicorp/terraform-plugin-sdk/v2`.
7. **Go module path** — `github.com/kreemer/terraform-provider-ghentapi`
8. **Tests** — unit tests for the token cache, JWT generation, and
   `EnsureOrgInstallation`; acceptance tests for resources using a mock HTTP
   server.

---

## Suggested Go package structure

```
internal/githubclient/
  auth.go     — generateJWT(appID, pemKey) string
                getInstallationToken(ctx, installationID) (string, error)
                TokenCache struct { ... }
  client.go   — ClientConfig struct
                NewClient(cfg ClientConfig) *Client
                Client.EnsureOrgInstallation(ctx, org) (string, error)
                Client.OrgToken(ctx, org) (string, error)
                Client.Do(ctx, method, path, body, headers) (*http.Response, error)
                Client.DoWithEnterpriseAuth(ctx, method, path, body) (*http.Response, error)
                Client.DoWithOrgAuth(ctx, org, method, path, body) (*http.Response, error)

internal/provider/
  provider.go                      — Schema, Configure, Resources, DataSources
  resource_org_setting.go
  datasource_installation_token.go
  testhelpers_test.go              — shared mock server helpers for tests
```

---

## Key implementation notes

### JWT generation (`auth.go`)

```go
import "github.com/golang-jwt/jwt/v5"

func generateJWT(appID string, pemKey []byte) (string, error) {
    key, err := jwt.ParseRSAPrivateKeyFromPEM(pemKey)
    // ...
    now := time.Now()
    claims := jwt.MapClaims{
        "iat": now.Add(-60 * time.Second).Unix(), // issued 60s ago (clock skew)
        "exp": now.Add(9 * time.Minute).Unix(),
        "iss": appID,
    }
    return jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
}
```

### Installation token cache (`auth.go`)

```go
type cachedToken struct {
    token     string
    expiresAt time.Time
}

type TokenCache struct {
    mu     sync.Mutex
    tokens map[string]cachedToken // key = installationID
}

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
```

### Implicit installation (`client.go`)

```go
func (c *Client) EnsureOrgInstallation(ctx context.Context, org string) (string, error) {
    c.orgInstallMu.Lock()
    if id, ok := c.orgInstallCache[org]; ok {
        c.orgInstallMu.Unlock()
        return id, nil
    }
    c.orgInstallMu.Unlock()

    slug, err := c.resolveEnterpriseSlug(ctx)  // cached after first call
    // ... list installations, find by client_id, install if needed ...
}
```

### Read() drift detection for `ghentapi_org_setting`

```go
// Unmarshal full API response, then keep only the keys in plan.Settings
var apiData map[string]interface{}
json.Unmarshal([]byte(body), &apiData)

result := make(map[string]string, len(plan.Settings.Elements()))
for k := range plan.Settings.Elements() {
    if v, ok := apiData[k]; ok {
        result[k] = fmt.Sprint(v)
    }
}
state.Settings = types.MapValueMust(types.StringType, /* convert result */)
```

