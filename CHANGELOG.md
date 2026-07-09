## 0.1.0 (Unreleased)

FEATURES:

* New resource `ghentapi_enterprise_org`: creates and manages a GitHub
  organisation inside an enterprise account. Organisation creation uses the
  GraphQL `createEnterpriseOrganization` mutation authenticated as the
  enterprise app. The org-level app is installed automatically into the new
  organisation after creation. Supports `terraform import` by organisation
  login. Destroy is a no-op (GitHub provides no delete API for organisations).
* New resource `ghentapi_org_setting`: manages GitHub organisation settings via
  `PATCH /orgs/{org}`. Only the keys present in the `settings` map are
  drift-checked; all other org fields are left untouched. Authenticated via
  the org-level app installation token.
* New data source `ghentapi_installation_token`: exposes a short-lived
  org-level installation token as a sensitive value for use with other
  providers. The token is re-evaluated on every plan/apply and is never
  stored in Terraform state.

PROVIDER:

* Two-app architecture: the provider requires two distinct GitHub Apps.
  - **Enterprise app** (`enterprise_app_id`): installed once at enterprise
    level; used to install the org app into organisations and to create new
    organisations via GraphQL.
  - **Org app** (`org_app_id`): installed per organisation; used to manage
    org settings and to vend installation tokens.
* Authenticates as both GitHub Apps using RSA private keys (PEM). Installation
  tokens are generated on demand and are never written to Terraform state.
* Implicit org-app installation: the org-level GitHub App is installed
  automatically the first time a resource targets a new organisation
  (controlled by `auto_install_org_app`, default `true`). The org →
  installation ID mapping is cached in memory for the Terraform run lifetime.
* New provider attribute `org_app_client_id` (required): the client ID of the
  org-level GitHub App, used to look up or create its installation in an org.
* New provider attribute `auto_install_org_app` (optional, default `true`):
  controls whether the org app is installed automatically when first needed.
* New provider attribute `repository_selection` (optional, default `"all"`):
  repository scope applied when auto-installing the org app (`"all"` or
  `"selected"`).
* PEM attributes (`enterprise_app_pem_file`, `org_app_pem_file`) accept either
  a raw PEM string or an absolute file path — the provider reads the file
  automatically if the value looks like a path.
* Supports GitHub.com and GitHub Enterprise Server via the `base_url`
  attribute.
* In-memory token cache with a 5-minute safety margin to avoid using
  near-expiry tokens.
* Automatic retries on HTTP 429 and 5xx responses (up to 3 times, exponential
  back-off).
