// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/kreemer/terraform-provider-ghentapi/internal/githubclient"
)

var _ datasource.DataSource = &organizationMembersDataSource{}

func NewOrganizationMembersDataSource() datasource.DataSource {
	return &organizationMembersDataSource{}
}

type organizationMembersDataSource struct {
	client *githubclient.Client
}

var organizationMemberAttrTypes = map[string]attr.Type{
	"login":    types.StringType,
	"email":    types.StringType,
	"is_owner": types.BoolType,
}

type organizationMembersModel struct {
	Organization types.String `tfsdk:"organization"`
	Members      types.List   `tfsdk:"members"`
}

func (d *organizationMembersDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_organization_members"
}

func (d *organizationMembersDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the members of a GitHub organisation, including their login, publicly visible email (if any), and whether they hold the owner (admin) role. " +
			"GitHub's API only ever exposes a member's *public* profile email; private or verified emails are never returned, so `email` may be empty for members who haven't set one public.",
		Attributes: map[string]schema.Attribute{
			"organization": schema.StringAttribute{
				MarkdownDescription: "GitHub organisation login.",
				Required:            true,
			},
			"members": schema.ListNestedAttribute{
				MarkdownDescription: "The list of organisation members.",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"login": schema.StringAttribute{
							MarkdownDescription: "The member's GitHub handle.",
							Computed:            true,
						},
						"email": schema.StringAttribute{
							MarkdownDescription: "The member's publicly visible profile email, if any.",
							Computed:            true,
						},
						"is_owner": schema.BoolAttribute{
							MarkdownDescription: "Whether the member holds the organisation owner (admin) role.",
							Computed:            true,
						},
					},
				},
			},
		},
	}
}

func (d *organizationMembersDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *organizationMembersDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config organizationMembersModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	org := config.Organization.ValueString()
	members, err := d.client.ListOrganizationMembers(ctx, org)
	if err != nil {
		resp.Diagnostics.AddError("Error listing organization members", err.Error())
		return
	}

	memberValues := make([]attr.Value, 0, len(members))
	for _, m := range members {
		objValue, diags := types.ObjectValue(organizationMemberAttrTypes, map[string]attr.Value{
			"login":    types.StringValue(m.Login),
			"email":    types.StringValue(m.Email),
			"is_owner": types.BoolValue(m.IsOwner),
		})
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		memberValues = append(memberValues, objValue)
	}

	membersList, diags := types.ListValue(types.ObjectType{AttrTypes: organizationMemberAttrTypes}, memberValues)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &organizationMembersModel{
		Organization: config.Organization,
		Members:      membersList,
	})...)
}
