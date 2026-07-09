// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/kreemer/terraform-provider-ghentapi/internal/githubclient"
)

var _ provider.Provider = &GhentapiProvider{}

// GhentapiProvider defines the provider implementation.
type GhentapiProvider struct {
	// version is set to the provider version on release, "dev" when the
	// provider is built and ran locally, and "test" when running acceptance
	// testing.
	version string
}

// GhentapiProviderModel describes the provider data model.
type GhentapiProviderModel struct {
	BaseURL                     types.String `tfsdk:"base_url"`
	EnterpriseAppID             types.String `tfsdk:"enterprise_app_id"`
	EnterpriseAppInstallationID types.String `tfsdk:"enterprise_app_installation_id"`
	EnterpriseAppPemFile        types.String `tfsdk:"enterprise_app_pem_file"`
	OrgAppID                    types.String `tfsdk:"org_app_id"`
	OrgAppClientID              types.String `tfsdk:"org_app_client_id"`
	OrgAppPemFile               types.String `tfsdk:"org_app_pem_file"`
	AutoInstallOrgApp           types.Bool   `tfsdk:"auto_install_org_app"`
	RepositorySelection         types.String `tfsdk:"repository_selection"`
}

func (p *GhentapiProvider) Metadata(ctx context.Context, req provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "ghentapi"
	resp.Version = p.version
}

func (p *GhentapiProvider) Schema(ctx context.Context, req provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Provider for managing GitHub organisations via the GitHub REST API, authenticating as a GitHub App.",
		Attributes: map[string]schema.Attribute{
			"base_url": schema.StringAttribute{
				MarkdownDescription: "GitHub API base URL. Use `https://api.github.com` for GitHub.com or `https://{hostname}/api/v3` for GitHub Enterprise Server. Defaults to `https://api.github.com`.",
				Optional:            true,
			},
			"enterprise_app_id": schema.StringAttribute{
				MarkdownDescription: "App ID of the enterprise-level GitHub App used to install org-level apps.",
				Required:            true,
			},
			"enterprise_app_installation_id": schema.StringAttribute{
				MarkdownDescription: "Installation ID of the enterprise-level GitHub App.",
				Required:            true,
			},
			"enterprise_app_pem_file": schema.StringAttribute{
				MarkdownDescription: "PEM-encoded RSA private key for the enterprise-level GitHub App.",
				Required:            true,
				Sensitive:           true,
			},
			"org_app_id": schema.StringAttribute{
				MarkdownDescription: "App ID of the org-level GitHub App used to manage organisation settings.",
				Required:            true,
			},
			"org_app_client_id": schema.StringAttribute{
				MarkdownDescription: "Client ID of the org-level GitHub App. Required for installing the app into organisations.",
				Required:            true,
			},
			"org_app_pem_file": schema.StringAttribute{
				MarkdownDescription: "PEM-encoded RSA private key for the org-level GitHub App.",
				Required:            true,
				Sensitive:           true,
			},
			"auto_install_org_app": schema.BoolAttribute{
				MarkdownDescription: "When `true` (default), the org-level GitHub App is installed automatically into an organisation the first time a resource targets it. When `false`, an error is returned if the app is not already installed.",
				Optional:            true,
			},
			"repository_selection": schema.StringAttribute{
				MarkdownDescription: "Repository selection used when auto-installing the org app. Must be `all` or `selected`. Defaults to `all`.",
				Optional:            true,
			},
		},
	}
}

func (p *GhentapiProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var data GhentapiProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	baseURL := "https://api.github.com"
	if !data.BaseURL.IsNull() && !data.BaseURL.IsUnknown() && data.BaseURL.ValueString() != "" {
		baseURL = data.BaseURL.ValueString()
	}

	entPEM := resolvePEM(data.EnterpriseAppPemFile.ValueString())
	orgPEM := resolvePEM(data.OrgAppPemFile.ValueString())

	autoInstall := true
	if !data.AutoInstallOrgApp.IsNull() && !data.AutoInstallOrgApp.IsUnknown() {
		autoInstall = data.AutoInstallOrgApp.ValueBool()
	}

	repoSelection := ""
	if !data.RepositorySelection.IsNull() && !data.RepositorySelection.IsUnknown() {
		repoSelection = data.RepositorySelection.ValueString()
	}

	client := githubclient.NewClient(githubclient.ClientConfig{
		BaseURL:                     baseURL,
		EnterpriseAppID:             data.EnterpriseAppID.ValueString(),
		EnterpriseAppInstallationID: data.EnterpriseAppInstallationID.ValueString(),
		EnterpriseAppPEM:            entPEM,
		OrgAppID:                    data.OrgAppID.ValueString(),
		OrgAppClientID:              data.OrgAppClientID.ValueString(),
		OrgAppPEM:                   orgPEM,
		RepositorySelection:         repoSelection,
		AutoInstall:                 autoInstall,
	})

	resp.DataSourceData = client
	resp.ResourceData = client
}

// resolvePEM returns the raw PEM bytes. If the value looks like a file path
// and the file exists, its contents are read; otherwise the value itself is
// treated as the PEM data.
func resolvePEM(value string) []byte {
	if len(value) > 0 && value[0] == '/' || (len(value) > 1 && value[1] == ':') {
		data, err := os.ReadFile(value)
		if err == nil {
			return data
		}
	}
	return []byte(value)
}

func (p *GhentapiProvider) Resources(ctx context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewOrgSettingResource,
		NewEnterpriseOrgResource,
	}
}

func (p *GhentapiProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewInstallationTokenDataSource,
	}
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &GhentapiProvider{
			version: version,
		}
	}
}
