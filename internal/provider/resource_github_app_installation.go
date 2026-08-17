// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/kreemer/terraform-provider-ghentapi/internal/githubclient"
)

var _ resource.Resource = &GitHubAppInstallationResource{}
var _ resource.ResourceWithImportState = &GitHubAppInstallationResource{}

// NewGitHubAppInstallationResource returns a new instance of
// GitHubAppInstallationResource.
func NewGitHubAppInstallationResource() resource.Resource {
	return &GitHubAppInstallationResource{}
}

// GitHubAppInstallationResource installs an arbitrary, user-specified GitHub
// App into an organisation, authenticating with the enterprise app token.
type GitHubAppInstallationResource struct {
	client *githubclient.Client
}

// GitHubAppInstallationModel is the Terraform state model for
// ghentapi_github_app_installation.
type GitHubAppInstallationModel struct {
	Organization        types.String `tfsdk:"organization"`
	ClientID            types.String `tfsdk:"client_id"`
	RepositorySelection types.String `tfsdk:"repository_selection"`
	Repositories        types.List   `tfsdk:"repositories"`
	InstallationID      types.String `tfsdk:"installation_id"`
	AppSlug             types.String `tfsdk:"app_slug"`
}

func (r *GitHubAppInstallationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_github_app_installation"
}

func (r *GitHubAppInstallationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Installs a user-specified GitHub App into an organisation, authenticating with the " +
			"enterprise-level GitHub App token. This is distinct from the implicit installation of the " +
			"provider's own org app (see `ghentapi_org_setting` / `ghentapi_enterprise_org`): this resource " +
			"lets you install *any* GitHub App (identified by its client ID) into an organisation, and — " +
			"unlike the implicit org app installation — actually uninstalls the app on `terraform destroy`.\n\n" +
			"**Import:** `terraform import ghentapi_github_app_installation.example my-org/Iv2abc123aabbcc`, " +
			"using the organisation login and the app's client ID separated by a `/`.",

		Attributes: map[string]schema.Attribute{
			"organization": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "GitHub organisation login to install the app into. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"client_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Client ID of the GitHub App to install. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"repository_selection": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Repository selection for the installation. Must be `all`, `selected`, or `none`. " +
					"Defaults to `all`. `repositories` is required when this is `selected`.",
			},
			"repositories": schema.ListAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Names of the repositories (simple name, not `owner/repo`) the installation may access. Required when `repository_selection = \"selected\"`; ignored otherwise.",
			},
			"installation_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The installation ID assigned by GitHub.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"app_slug": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The URL-friendly slug of the installed GitHub App.",
			},
		},
	}
}

func (r *GitHubAppInstallationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*githubclient.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *githubclient.Client, got: %T.", req.ProviderData),
		)
		return
	}
	r.client = client
}

func (r *GitHubAppInstallationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan GitHubAppInstallationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	repositorySelection := "all"
	if !plan.RepositorySelection.IsNull() && !plan.RepositorySelection.IsUnknown() && plan.RepositorySelection.ValueString() != "" {
		repositorySelection = plan.RepositorySelection.ValueString()
	}

	repositories, diags := repositoriesFromList(ctx, plan.Repositories)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !validateRepositorySelection(repositorySelection, repositories, &resp.Diagnostics) {
		return
	}

	installation, err := r.client.InstallGitHubApp(ctx, plan.Organization.ValueString(), plan.ClientID.ValueString(), repositorySelection, repositories)
	if err != nil {
		resp.Diagnostics.AddError("Error installing GitHub App", err.Error())
		return
	}

	r.populateModel(&plan, installation)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *GitHubAppInstallationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state GitHubAppInstallationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	installation, ok, err := r.client.FindGitHubAppInstallation(ctx, state.Organization.ValueString(), state.ClientID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error reading GitHub App installation", err.Error())
		return
	}
	if !ok {
		resp.State.RemoveResource(ctx)
		return
	}

	r.populateModel(&state, installation)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *GitHubAppInstallationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan GitHubAppInstallationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	repositorySelection := "all"
	if !plan.RepositorySelection.IsNull() && !plan.RepositorySelection.IsUnknown() && plan.RepositorySelection.ValueString() != "" {
		repositorySelection = plan.RepositorySelection.ValueString()
	}

	repositories, diags := repositoriesFromList(ctx, plan.Repositories)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !validateRepositorySelection(repositorySelection, repositories, &resp.Diagnostics) {
		return
	}

	// GitHub's install endpoint is an upsert: calling it again for an
	// already-installed app updates its repository selection/access.
	installation, err := r.client.InstallGitHubApp(ctx, plan.Organization.ValueString(), plan.ClientID.ValueString(), repositorySelection, repositories)
	if err != nil {
		resp.Diagnostics.AddError("Error updating GitHub App installation", err.Error())
		return
	}

	r.populateModel(&plan, installation)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *GitHubAppInstallationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state GitHubAppInstallationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.UninstallGitHubApp(ctx, state.Organization.ValueString(), state.InstallationID.ValueString()); err != nil {
		resp.Diagnostics.AddError("Error uninstalling GitHub App", err.Error())
		return
	}
}

// ImportState imports an existing installation into Terraform state.
//
// Usage: terraform import ghentapi_github_app_installation.example my-org/Iv2abc123aabbcc.
func (r *GitHubAppInstallationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		resp.Diagnostics.AddError(
			"Unexpected Import Identifier",
			fmt.Sprintf("Expected import identifier of the form \"organization/client_id\", got: %q", req.ID),
		)
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("organization"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("client_id"), parts[1])...)
}

// populateModel copies the result of an install/find operation into the model.
func (r *GitHubAppInstallationResource) populateModel(model *GitHubAppInstallationModel, installation githubclient.AppInstallation) {
	model.InstallationID = types.StringValue(installation.ID)
	model.AppSlug = types.StringValue(installation.AppSlug)
	if installation.RepositorySelection != "" {
		model.RepositorySelection = types.StringValue(installation.RepositorySelection)
	}
}

// repositoriesFromList converts the repositories list attribute into a []string.
func repositoriesFromList(ctx context.Context, list types.List) ([]string, diag.Diagnostics) {
	if list.IsNull() || list.IsUnknown() {
		return nil, nil
	}
	repositories := make([]string, 0, len(list.Elements()))
	diags := list.ElementsAs(ctx, &repositories, false)
	return repositories, diags
}

// validateRepositorySelection enforces that repositories is set when
// repository_selection is "selected", and that repository_selection is one
// of the values GitHub's API accepts.
func validateRepositorySelection(repositorySelection string, repositories []string, diags *diag.Diagnostics) bool {
	switch repositorySelection {
	case "all", "selected", "none":
	default:
		diags.AddError(
			"Invalid repository_selection",
			fmt.Sprintf("repository_selection must be \"all\", \"selected\", or \"none\", got: %q", repositorySelection),
		)
		return false
	}

	if repositorySelection == "selected" && len(repositories) == 0 {
		diags.AddError(
			"Missing repositories",
			"repositories must be set to a non-empty list when repository_selection is \"selected\".",
		)
		return false
	}

	return true
}
