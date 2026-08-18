// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/kreemer/terraform-provider-ghentapi/internal/githubclient"
)

var _ resource.Resource = &CostCenterResource{}
var _ resource.ResourceWithImportState = &CostCenterResource{}

// costCenterResourceTypeUser, etc. are the resource "type" values used by the
// GitHub cost center API.
const (
	costCenterResourceTypeUser         = "User"
	costCenterResourceTypeOrganization = "Organization"
	costCenterResourceTypeRepository   = "Repository"
	costCenterResourceTypeTeam         = "Team"
)

// NewCostCenterResource returns a new instance of CostCenterResource.
func NewCostCenterResource() resource.Resource {
	return &CostCenterResource{}
}

// CostCenterResource manages an enterprise billing cost center, authenticated
// with the enterprise fine-grained personal access token (the cost center
// billing API does not support GitHub App authentication).
type CostCenterResource struct {
	client *githubclient.Client
}

// CostCenterModel is the Terraform state model for ghentapi_cost_center.
type CostCenterModel struct {
	ID                  types.String `tfsdk:"id"`
	Name                types.String `tfsdk:"name"`
	AICreditPoolEnabled types.Bool   `tfsdk:"ai_credit_pool_enabled"`
	State               types.String `tfsdk:"state"`
	Users               types.Set    `tfsdk:"users"`
	Organizations       types.Set    `tfsdk:"organizations"`
	Repositories        types.Set    `tfsdk:"repositories"`
	EnterpriseTeams     types.Set    `tfsdk:"enterprise_teams"`
}

func (r *CostCenterResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cost_center"
}

func (r *CostCenterResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Creates and manages a GitHub Enterprise billing cost center. Cost centers group " +
			"users, organizations, repositories, and enterprise teams together so their usage is billed " +
			"against a shared budget.\n\n" +
			"**Import:** `terraform import ghentapi_cost_center.example COST_CENTER_ID`.\n\n" +
			"> **Note:** GitHub provides no way to permanently delete a cost center. `terraform destroy` calls " +
			"the archive API, which sets the cost center's `state` to `deleted`; the cost center still exists " +
			"on GitHub in an archived state.\n\n" +
			"> **Note:** The cost center billing API does not support GitHub App authentication. The " +
			"`enterprise_fine_grained_token` provider attribute must be set to a fine-grained (or classic) " +
			"personal access token for this resource to work.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The ID of the cost center, assigned by GitHub.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The name of the cost center (max length 255 characters).",
			},
			"ai_credit_pool_enabled": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Whether the cost center draws from the AI credit pool. Can only be enabled " +
					"for cost centers that contain only user or team resources. Defaults to `false`.",
			},
			"state": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The state of the cost center: `active` or `deleted`.",
			},
			"users": schema.SetAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				Default:             setdefault.StaticValue(types.SetValueMust(types.StringType, []attr.Value{})),
				MarkdownDescription: "Usernames of the users assigned to this cost center.",
			},
			"organizations": schema.SetAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				Default:             setdefault.StaticValue(types.SetValueMust(types.StringType, []attr.Value{})),
				MarkdownDescription: "Logins of the organizations assigned to this cost center.",
			},
			"repositories": schema.SetAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				Default:             setdefault.StaticValue(types.SetValueMust(types.StringType, []attr.Value{})),
				MarkdownDescription: "Names of the repositories (`owner/repo`) assigned to this cost center.",
			},
			"enterprise_teams": schema.SetAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				Default:             setdefault.StaticValue(types.SetValueMust(types.StringType, []attr.Value{})),
				MarkdownDescription: "Slugs of the enterprise teams assigned to this cost center.",
			},
		},
	}
}

func (r *CostCenterResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *CostCenterResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan CostCenterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	cc, err := r.client.CreateCostCenter(ctx, plan.Name.ValueString(), plan.AICreditPoolEnabled.ValueBool())
	if err != nil {
		resp.Diagnostics.AddError("Error creating cost center", err.Error())
		return
	}
	plan.ID = types.StringValue(cc.ID)

	changes, diags := modelToResourceChanges(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.AddCostCenterResources(ctx, cc.ID, changes); err != nil {
		resp.Diagnostics.AddError("Error assigning resources to cost center", err.Error())
		return
	}

	removed := r.readIntoModel(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if removed {
		resp.Diagnostics.AddError("Error reading cost center after creation", "Cost center was not found immediately after creation.")
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *CostCenterResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state CostCenterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	removed := r.readIntoModel(ctx, &state, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if removed {
		resp.State.RemoveResource(ctx)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *CostCenterResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state CostCenterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	costCenterID := state.ID.ValueString()
	plan.ID = state.ID

	if plan.Name.ValueString() != state.Name.ValueString() || plan.AICreditPoolEnabled.ValueBool() != state.AICreditPoolEnabled.ValueBool() {
		name := plan.Name.ValueString()
		aiEnabled := plan.AICreditPoolEnabled.ValueBool()
		if _, err := r.client.UpdateCostCenter(ctx, costCenterID, &name, &aiEnabled); err != nil {
			resp.Diagnostics.AddError("Error updating cost center", err.Error())
			return
		}
	}

	added, removed, diags := diffResourceChanges(ctx, &state, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.AddCostCenterResources(ctx, costCenterID, added); err != nil {
		resp.Diagnostics.AddError("Error assigning resources to cost center", err.Error())
		return
	}
	if err := r.client.RemoveCostCenterResources(ctx, costCenterID, removed); err != nil {
		resp.Diagnostics.AddError("Error unassigning resources from cost center", err.Error())
		return
	}

	removedAfterUpdate := r.readIntoModel(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if removedAfterUpdate {
		resp.Diagnostics.AddError("Error reading cost center after update", "Cost center was not found immediately after update.")
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete archives the cost center via the GitHub API. GitHub provides no way
// to permanently delete a cost center.
func (r *CostCenterResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state CostCenterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.DeleteCostCenter(ctx, state.ID.ValueString()); err != nil {
		resp.Diagnostics.AddError("Error archiving cost center", err.Error())
		return
	}
	tflog.Info(ctx, "Archived cost center "+state.ID.ValueString()+" ("+state.Name.ValueString()+"); it still exists on GitHub in a deleted state.")
}

// ImportState imports an existing cost center into Terraform state by its ID.
//
// Usage: terraform import ghentapi_cost_center.example COST_CENTER_ID.
func (r *CostCenterResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// readIntoModel calls GetCostCenter and updates model in place. Returns true
// if the cost center was not found (caller should remove from state).
func (r *CostCenterResource) readIntoModel(ctx context.Context, model *CostCenterModel, diags interface {
	HasError() bool
	AddError(string, string)
}) (removed bool) {
	costCenterID := model.ID.ValueString()
	cc, ok, err := r.client.GetCostCenter(ctx, costCenterID)
	if err != nil {
		diags.AddError("Error reading cost center", err.Error())
		return false
	}
	if !ok {
		tflog.Warn(ctx, "Cost center "+costCenterID+" not found; removing from state.")
		return true
	}

	model.ID = types.StringValue(cc.ID)
	model.Name = types.StringValue(cc.Name)
	model.AICreditPoolEnabled = types.BoolValue(cc.AICreditPoolEnabled)
	model.State = types.StringValue(cc.State)

	users, orgs, repos, teams := bucketResourcesByType(cc.Resources)
	model.Users = types.SetValueMust(types.StringType, users)
	model.Organizations = types.SetValueMust(types.StringType, orgs)
	model.Repositories = types.SetValueMust(types.StringType, repos)
	model.EnterpriseTeams = types.SetValueMust(types.StringType, teams)

	return false
}

// bucketResourcesByType groups a cost center's resources into attr.Value
// slices per Terraform attribute.
func bucketResourcesByType(resources []githubclient.CostCenterResource) (users, orgs, repos, teams []attr.Value) {
	for _, res := range resources {
		v := types.StringValue(res.Name)
		switch res.Type {
		case costCenterResourceTypeUser:
			users = append(users, v)
		case costCenterResourceTypeOrganization:
			orgs = append(orgs, v)
		case costCenterResourceTypeRepository:
			repos = append(repos, v)
		case costCenterResourceTypeTeam:
			teams = append(teams, v)
		}
	}
	return users, orgs, repos, teams
}

// modelToResourceChanges converts all resource sets in model into a single
// CostCenterResourceChanges, used when creating a cost center.
func modelToResourceChanges(ctx context.Context, model *CostCenterModel) (githubclient.CostCenterResourceChanges, diag.Diagnostics) {
	var diags diag.Diagnostics
	var changes githubclient.CostCenterResourceChanges

	diags.Append(model.Users.ElementsAs(ctx, &changes.Users, false)...)
	diags.Append(model.Organizations.ElementsAs(ctx, &changes.Organizations, false)...)
	diags.Append(model.Repositories.ElementsAs(ctx, &changes.Repositories, false)...)
	diags.Append(model.EnterpriseTeams.ElementsAs(ctx, &changes.EnterpriseTeams, false)...)

	return changes, diags
}

// diffResourceChanges computes which resources must be added and removed to
// move from the current state's resource sets to the plan's resource sets.
func diffResourceChanges(ctx context.Context, state, plan *CostCenterModel) (added, removed githubclient.CostCenterResourceChanges, diags diag.Diagnostics) {
	stateChanges, d := modelToResourceChanges(ctx, state)
	diags.Append(d...)
	planChanges, d := modelToResourceChanges(ctx, plan)
	diags.Append(d...)
	if diags.HasError() {
		return added, removed, diags
	}

	added = githubclient.CostCenterResourceChanges{
		Users:           setDiff(planChanges.Users, stateChanges.Users),
		Organizations:   setDiff(planChanges.Organizations, stateChanges.Organizations),
		Repositories:    setDiff(planChanges.Repositories, stateChanges.Repositories),
		EnterpriseTeams: setDiff(planChanges.EnterpriseTeams, stateChanges.EnterpriseTeams),
	}
	removed = githubclient.CostCenterResourceChanges{
		Users:           setDiff(stateChanges.Users, planChanges.Users),
		Organizations:   setDiff(stateChanges.Organizations, planChanges.Organizations),
		Repositories:    setDiff(stateChanges.Repositories, planChanges.Repositories),
		EnterpriseTeams: setDiff(stateChanges.EnterpriseTeams, planChanges.EnterpriseTeams),
	}
	return added, removed, diags
}

// setDiff returns the elements of a that are not present in b.
func setDiff(a, b []string) []string {
	inB := make(map[string]struct{}, len(b))
	for _, v := range b {
		inB[v] = struct{}{}
	}
	var diff []string
	for _, v := range a {
		if _, ok := inB[v]; !ok {
			diff = append(diff, v)
		}
	}
	return diff
}
