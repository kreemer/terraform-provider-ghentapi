## 0.1.0 (Unreleased)

FEATURES:

* New resource `ghentapi_org_setting`: manages GitHub organisation settings via
  `PATCH /orgs/{org}`. Only the keys present in the `settings` map are
  drift-checked; all other org fields are left untouched.
* New data source `ghentapi_installation_token`: exposes a short-lived
  installation token for an organisation as a sensitive value for use with
  other providers.

PROVIDER:

* Authenticates as a GitHub App (enterprise-level and org-level) using RSA
  private keys. Installation tokens are generated on demand and are never
  written to Terraform state.
* Implicit org-app installation: the org-level GitHub App is installed
  automatically the first time a resource targets a new organisation
  (controlled by `auto_install_org_app`, default `true`).
* Supports GitHub.com and GitHub Enterprise Server via the `base_url`
  attribute.
* In-memory token cache with a 5-minute safety margin to avoid using
  near-expiry tokens.
* Automatic retries on HTTP 429 and 5xx responses (up to 3 times, exponential
  back-off).
