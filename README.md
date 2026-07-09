# terraform-provider-ghentapi

A Terraform provider for managing GitHub organisations via the GitHub REST API,
authenticating natively as a **GitHub App**. Installation tokens (1-hour expiry)
are generated on demand and are **never** written to Terraform state, eliminating
the `401 Bad credentials` errors that occur when tokens expire between the refresh
and apply phases.

## Why a new provider?

The standard approach of injecting `Authorization` headers via a generic REST
provider breaks during Terraform's refresh phase because data-source values
(including freshly generated tokens) are resolved *after* all `Read()` calls
complete. This provider generates tokens itself at request time — no token ever
touches state.

## Features

- Authenticates as a **GitHub App** using a private RSA key (no Personal Access Tokens).
- **Implicit org-app installation**: automatically installs the org-level GitHub App
  into a new organisation the first time it is referenced (configurable via
  `auto_install_org_app`).
- In-memory token cache with a 5-minute safety margin before expiry — no extra
  API calls within a single Terraform run.
- Supports both **GitHub.com** and **GitHub Enterprise Server** via `base_url`.
- Automatic retries on HTTP 429 and 5xx responses (up to 3 times, exponential back-off).
- Built on the **Terraform Plugin Framework** (not the legacy SDK).

## Requirements

- [Terraform](https://developer.hashicorp.com/terraform/downloads) >= 1.11
- [Go](https://golang.org/doc/install) >= 1.24 (for building from source)

## Building the Provider

```shell
git clone https://github.com/kreemer/terraform-provider-ghentapi
cd terraform-provider-ghentapi
go install
```

## Using the Provider

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
  base_url = "https://api.github.com"

  enterprise_app_id              = var.ent_app_id
  enterprise_app_installation_id = var.ent_app_installation_id
  enterprise_app_pem_file        = var.ent_pem

  org_app_id        = var.org_app_id
  org_app_client_id = var.org_app_client_id
  org_app_pem_file  = var.org_pem
}

resource "ghentapi_org_setting" "this" {
  for_each = toset(var.organizations)

  organization = each.key
  settings = {
    billing_email = "github@example.com"
  }
}

# Obtain a per-org installation token for use with other providers.
data "ghentapi_installation_token" "org" {
  for_each     = toset(var.organizations)
  organization = each.key
}
```

See the [`examples/`](./examples/) directory for complete configuration examples.

## Resources & Data Sources

| Type | Name | Description |
|------|------|-------------|
| Resource | `ghentapi_org_setting` | Manages a set of GitHub org-level settings via `PATCH /orgs/{org}`. Only the keys present in `settings` are drift-checked; all other fields are ignored. |
| Data Source | `ghentapi_installation_token` | Exposes a fresh org-level installation token as a sensitive value for use with other providers. |

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

