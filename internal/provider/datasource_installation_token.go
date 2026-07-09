// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/kreemer/terraform-provider-ghentapi/internal/githubclient"
)

var _ datasource.DataSource = &installationTokenDataSource{}

func NewInstallationTokenDataSource() datasource.DataSource {
	return &installationTokenDataSource{}
}

type installationTokenDataSource struct {
	client *githubclient.Client
}

type installationTokenModel struct {
	Organization types.String `tfsdk:"organization"`
	Token        types.String `tfsdk:"token"`
}

func (d *installationTokenDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_installation_token"
}

func (d *installationTokenDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Returns a short-lived org app installation token for the given organisation. The token is sensitive and is never stored in Terraform state.",
		Attributes: map[string]schema.Attribute{
			"organization": schema.StringAttribute{
				MarkdownDescription: "GitHub organisation login.",
				Required:            true,
			},
			"token": schema.StringAttribute{
				MarkdownDescription: "Short-lived GitHub App installation token.",
				Computed:            true,
				Sensitive:           true,
			},
		},
	}
}

func (d *installationTokenDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*githubclient.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data type",
			fmt.Sprintf("Expected *githubclient.Client, got %T", req.ProviderData))
		return
	}
	d.client = client
}

func (d *installationTokenDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config installationTokenModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	token, err := d.client.OrgToken(ctx, config.Organization.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error obtaining installation token", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &installationTokenModel{
		Organization: config.Organization,
		Token:        types.StringValue(token),
	})...)
}
