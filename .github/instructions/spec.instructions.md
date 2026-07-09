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
App will install the org-app within the organisation by a special resource.

See in https://docs.github.com/en/enterprise-cloud@latest/admin/managing-github-apps-for-your-enterprise/automate-installations
the official documentation about how to install enterprise/org apps.

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
│   │   ├── auth.go          # GitHub App JWT + installation token generation
│   │   └── client.go        # HTTP client wrapper (retries, base URL switching)
│   └── provider/
│       ├── provider.go      # Provider schema + configuration
│       ├── resource_org_app_installation.go
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
  org_app_id      = var.org_app_id
  org_app_client_id = var.org_app_client_id
  org_app_pem_file = var.org_pem   # sensitive, raw PEM string
}
```

### Token generation (internal detail, not stored in state)

For every API call the provider:
1. Signs a JWT using the relevant app's private key (RS256, 10-minute expiry).
2. Calls `POST /app/installations/{installation_id}/access_tokens` to obtain a
   fresh installation token.
3. Caches the token in memory (with its `expires_at` timestamp) for the
   duration of a single Terraform run.  The cache is invalidated 5 minutes
   before expiry to give a safety margin.
4. **Never writes any token to Terraform state.**

---

## Resource: `ghentapi_org_app_installation`

Installs the org-level GitHub App into an organisation via the enterprise API.
Equivalent to the current `restapi_object.org_app_installation`.

```hcl
resource "ghentapi_org_app_installation" "this" {
  for_each = toset(var.organizations)

  enterprise_slug      = "university-of-bern"
  organization         = each.key
  org_app_client_id    = var.org_app_client_id

  # "all" or "selected"
  repository_selection = "all"
}
```

### Computed attributes

| Attribute        | Type   | Description                                      |
|-----------------|--------|--------------------------------------------------|
| `installation_id` | string | The numeric installation ID returned by the API |

### API mapping

| Terraform lifecycle | HTTP method | Path                                                                                     |
|--------------------|-------------|------------------------------------------------------------------------------------------|
| Create             | POST        | `/enterprises/{enterprise}/apps/organizations/{org}/installations`                      |
| Read               | GET         | `/enterprises/{enterprise}/apps/organizations/{org}/installations` (search by client_id)|
| Delete             | DELETE      | `/enterprises/{enterprise}/apps/organizations/{org}/installations/{installation_id}`    |
| Update             | force-new   | Any change to `org_app_client_id` triggers destroy + create                              |

The Read uses a list + search pattern (search for the installation where
`client_id == org_app_client_id`) because the GitHub API does not have a
single-item GET by client_id.

Fields returned by the API that are NOT in the resource schema
(`app_slug`, `created_at`, `updated_at`, `events`, `permissions`,
`repositories_url`) must be ignored during drift detection.

### Authentication

Uses the **enterprise app** installation token (from provider config).

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

  # The installation token for this org is obtained automatically from
  # ghentapi_org_app_installation.this[each.key].installation_id
  installation_id = ghentapi_org_app_installation.this[each.key].installation_id
}
```

### Attributes

| Attribute         | Type        | Description                                               |
|------------------|-------------|-----------------------------------------------------------|
| `organization`   | string      | GitHub organisation login                                |
| `settings`       | map(string) | Keys/values to manage.  Only these are drift-checked.    |
| `installation_id`| string      | Installation ID used to obtain the per-org token.         |
| `api_response`   | string      | Full JSON body from the last successful GET (for debug). |

### API mapping

| Terraform lifecycle | HTTP method | Path              |
|--------------------|-------------|-------------------|
| Create / Update    | PATCH       | `/orgs/{org}`    |
| Read               | GET         | `/orgs/{org}`    |
| Delete             | no-op       | (settings can't be "deleted", resource is just removed from state) |

### Authentication

Uses the **org app** installation token, obtained using `installation_id`.
The token is generated fresh per API call (cached in memory, never in state).

### Drift detection

After `Read()`, only the keys present in `settings` are compared against the
API response.  Extra fields returned by the API are silently ignored.

---

## Data source: `ghentapi_installation_token` (optional / nice to have)

Exposes the installation token as a data source so it can be passed to other
providers if needed.  The token is marked `sensitive = true` and is **not**
stored in state (use `ephemeral` resource semantics if available in the target
Terraform version, otherwise sensitive output).

```hcl
data "ghentapi_installation_token" "org" {
  for_each        = toset(var.organizations)
  installation_id = ghentapi_org_app_installation.this[each.key].installation_id
}
```

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
  org_app_pem_file               = var.org_pem
}

resource "ghentapi_org_app_installation" "this" {
  for_each             = toset(var.organizations)
  enterprise_slug      = "university-of-bern"
  organization         = each.key
  org_app_client_id    = var.org_app_client_id
  repository_selection = "all"
}

resource "ghentapi_org_setting" "this" {
  for_each        = toset(var.organizations)
  organization    = each.key
  installation_id = ghentapi_org_app_installation.this[each.key].installation_id
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
3. **GHES + GHEC** — the `base_url` provider attribute selects the endpoint;
   no compile-time switch.
4. **Retries** — on HTTP 429 or 5xx, retry up to 3 times with exponential
   back-off.
5. **Terraform Plugin Framework** — use `hashicorp/terraform-plugin-framework`,
   not the legacy `hashicorp/terraform-plugin-sdk/v2`.
6. **Go module path** — `github.com/kreemer/terraform-provider-ghentapi`
7. **Tests** — unit tests for the token cache and JWT generation; acceptance
   tests for the two resources using a mock HTTP server (pattern from
   `hashicorp/terraform-plugin-framework`'s `resource.UnitTest`).

---

## Suggested Go package structure

```
internal/githubclient/
  auth.go          — generateJWT(appID, pemKey) string
                     getInstallationToken(ctx, installationID) (string, error)
                     tokenCache struct { ... }
  client.go        — NewClient(baseURL, ...) *Client
                     Client.Do(ctx, method, path, body) (*http.Response, error)

internal/provider/
  provider.go      — Schema, Configure, Resources, DataSources
  resource_org_app_installation.go
  resource_org_setting.go
  datasource_installation_token.go
  testhelpers_test.go   — shared mock server helpers for tests
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
