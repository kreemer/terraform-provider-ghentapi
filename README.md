# terraform-provider-ghentapi

A Terraform provider for managing GitHub organisations via the GitHub REST API,
authenticating natively via **two GitHub Apps** (one enterprise-level, one
org-level). Installation tokens (1-hour expiry) are generated on demand and are
**never** written to Terraform state, eliminating the `401 Bad credentials`
errors that occur when tokens expire between the refresh and apply phases.

## Two-App Architecture

This provider is designed around two distinct GitHub Apps:

```
┌─────────────────────────────────────┐
│         Enterprise GitHub App        │
│  (enterprise_app_id + PEM key)       │
│                                      │
│  • Installed once at enterprise level│
│  • Installs the org app into orgs    │
│  • Creates new organisations via     │
│    GraphQL (createEnterpriseOrg)     │
└────────────────┬────────────────────┘
                 │ auto-installs on first use
                 ▼
┌─────────────────────────────────────┐
│           Org GitHub App            │
│  (org_app_id + PEM key)             │
│                                     │
│  • Installed per organisation       │
│  • Manages org settings (PATCH)     │
│  • Provides installation tokens     │
│    for use with other providers     │
└─────────────────────────────────────┘
```

The first time a resource targets an organisation, the provider calls
`EnsureOrgInstallation`: it looks up the org app installation by `client_id`,
and if it is not found yet and `auto_install_org_app = true`, installs it
automatically. The org → installation ID mapping is cached in memory for the
lifetime of the Terraform run.

## Features

- Authenticates as two **GitHub Apps** using RSA private keys (no Personal Access Tokens).
- **Enterprise app**: creates organisations via GraphQL and installs the org app into them.
- **Org app**: manages organisation settings and vends installation tokens.
- **Implicit org-app installation**: the org app is installed automatically the first
  time a resource targets an organisation (controlled by `auto_install_org_app`).
- In-memory token cache with a 5-minute safety margin before expiry — no redundant
  API calls within a single Terraform run.
- Supports both **GitHub.com** and **GitHub Enterprise Server** via `base_url`.
- Automatic retries on HTTP 429 and 5xx responses (up to 3 times, exponential back-off).
- Built on the **Terraform Plugin Framework** (not the legacy SDK).

## Requirements

- [Terraform](https://developer.hashicorp.com/terraform/downloads) >= 1.11
- [Go](https://golang.org/doc/install) >= 1.24 (for building from source)

## Documentation

Full provider, resource, and data source documentation is available on the
[Terraform Registry](https://registry.terraform.io/providers/kreemer/ghentapi/latest/docs).

## Building the Provider

```shell
git clone https://github.com/kreemer/terraform-provider-ghentapi
cd terraform-provider-ghentapi
go install
```

## Developing the Provider

Run the unit tests (no GitHub credentials required):

```shell
go test ./...
```

Run linting:

```shell
golangci-lint run
```

Generate documentation:

```shell
make generate
```

