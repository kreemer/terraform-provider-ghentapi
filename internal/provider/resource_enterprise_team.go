// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/kreemer/terraform-provider-ghentapi/internal/githubclient"
)

var _ resource.Resource = &EnterpriseTeamResource{}
var _ resource.ResourceWithImportState = &EnterpriseTeamResource{}

// enterpriseTeamOrganizationSelectionTypes and
// enterpriseTeamNotificationSettings hold the values accepted by the GitHub
// API for the corresponding attributes.
var (
	enterpriseTeamOrganizationSelectionTypes = []string{"disabled", "selected", "all"}
	enterpriseTeamNotificationSettings       = []string{"notifications_enabled", "notifications_disabled"}
)

// NewEnterpriseTeamResource returns a new instance of EnterpriseTeamResource.
func NewEnterpriseTeamResource() resource.Resource {
	return &EnterpriseTeamResource{}
}

// EnterpriseTeamResource manages a GitHub Enterprise Team, authenticated with
// the enterprise-level GitHub App. Team membership is intentionally not
// managed by this resource.
type EnterpriseTeamResource struct {
	client *githubclient.Client
}

// EnterpriseTeamModel is the Terraform state model for
// ghentapi_enterprise_team.
type EnterpriseTeamModel struct {
	ID                        types.String `tfsdk:"id"`
	Slug                      types.String `tfsdk:"slug"`
	Name                      types.String `tfsdk:"name"`
	Description               types.String `tfsdk:"description"`
	OrganizationSelectionType types.String `tfsdk:"organization_selection_type"`
	GroupID                   types.String `tfsdk:"group_id"`
	NotificationSetting       types.String `tfsdk:"notification_setting"`
}

func (r *EnterpriseTeamResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_enterprise_team"
}

func (r *EnterpriseTeamResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Creates and manages a GitHub Enterprise Team, authenticated with the " +
			"enterprise-level GitHub App. Enterprise teams can be referenced by their `slug` from " +
			"`ghentapi_cost_center.enterprise_teams` to group their usage under a shared billing budget.\n\n" +
			"**Import:** `terraform import ghentapi_enterprise_team.example TEAM_SLUG`.\n\n" +
			"> **Note:** This resource only manages the team itself (name, description, organisation " +
			"assignment, IdP group linkage, and notification setting). Team membership " +
			"(`/enterprises/{enterprise}/teams/{team}/memberships`) is not managed by this resource.",

		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The numeric ID of the enterprise team, assigned by GitHub.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"slug": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The slug GitHub generates from the team's `name` (prefixed with `ent:`). " +
					"Renaming the team changes its slug; the new value is reflected here after `terraform apply`.",
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The name of the team. Changing this renames the team on GitHub and changes its `slug`.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "A description of the team.",
			},
			"organization_selection_type": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Which organisations in the enterprise have access to this team. One of " +
					"`disabled` (not assigned to any organisation, the default), `selected`, or `all`.",
			},
			"group_id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The ID of the IdP group to assign team membership with. Leave unset to manage membership directly.",
			},
			"notification_setting": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Whether team members receive notifications when the team is @mentioned. One of " +
					"`notifications_enabled` (the default) or `notifications_disabled`.",
			},
		},
	}
}

func (r *EnterpriseTeamResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *EnterpriseTeamResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan EnterpriseTeamModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !validateEnterpriseTeamChoices(&plan, &resp.Diagnostics) {
		return
	}

	team, err := r.client.CreateEnterpriseTeam(ctx, modelToEnterpriseTeamInput(&plan, true))
	if err != nil {
		resp.Diagnostics.AddError("Error creating enterprise team", err.Error())
		return
	}

	enterpriseTeamToModel(&plan, team)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *EnterpriseTeamResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state EnterpriseTeamModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	team, ok, err := r.client.GetEnterpriseTeam(ctx, state.Slug.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error reading enterprise team", err.Error())
		return
	}
	if !ok {
		tflog.Warn(ctx, "Enterprise team "+state.Slug.ValueString()+" not found; removing from state.")
		resp.State.RemoveResource(ctx)
		return
	}

	enterpriseTeamToModel(&state, team)

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *EnterpriseTeamResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state EnterpriseTeamModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !validateEnterpriseTeamChoices(&plan, &resp.Diagnostics) {
		return
	}

	// Renaming the team changes its slug, so the update must be addressed by
	// the slug currently in state, not the (possibly new) name in plan.
	team, err := r.client.UpdateEnterpriseTeam(ctx, state.Slug.ValueString(), modelToEnterpriseTeamInput(&plan, true))
	if err != nil {
		resp.Diagnostics.AddError("Error updating enterprise team", err.Error())
		return
	}

	plan.ID = state.ID
	enterpriseTeamToModel(&plan, team)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *EnterpriseTeamResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state EnterpriseTeamModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.DeleteEnterpriseTeam(ctx, state.Slug.ValueString()); err != nil {
		resp.Diagnostics.AddError("Error deleting enterprise team", err.Error())
		return
	}
	tflog.Info(ctx, "Deleted enterprise team "+state.Slug.ValueString())
}

// ImportState imports an existing enterprise team into Terraform state by its
// slug.
//
// Usage: terraform import ghentapi_enterprise_team.example TEAM_SLUG.
func (r *EnterpriseTeamResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("slug"), req, resp)
}

// validateEnterpriseTeamChoices checks that organization_selection_type and
// notification_setting, if set, are one of the values GitHub accepts.
func validateEnterpriseTeamChoices(model *EnterpriseTeamModel, diags interface {
	AddAttributeError(path path.Path, summary, detail string)
	HasError() bool
}) bool {
	if v := model.OrganizationSelectionType; !v.IsNull() && !v.IsUnknown() && v.ValueString() != "" {
		if !contains(enterpriseTeamOrganizationSelectionTypes, v.ValueString()) {
			diags.AddAttributeError(
				path.Root("organization_selection_type"),
				"Invalid organization_selection_type",
				fmt.Sprintf("Must be one of %v, got %q.", enterpriseTeamOrganizationSelectionTypes, v.ValueString()),
			)
		}
	}
	if v := model.NotificationSetting; !v.IsNull() && !v.IsUnknown() && v.ValueString() != "" {
		if !contains(enterpriseTeamNotificationSettings, v.ValueString()) {
			diags.AddAttributeError(
				path.Root("notification_setting"),
				"Invalid notification_setting",
				fmt.Sprintf("Must be one of %v, got %q.", enterpriseTeamNotificationSettings, v.ValueString()),
			)
		}
	}
	return !diags.HasError()
}

// contains reports whether s is present in list.
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// modelToEnterpriseTeamInput converts model into a
// githubclient.EnterpriseTeamInput. includeName controls whether the name
// field is populated (both Create and Update always send the name).
func modelToEnterpriseTeamInput(model *EnterpriseTeamModel, includeName bool) githubclient.EnterpriseTeamInput {
	input := githubclient.EnterpriseTeamInput{
		Description:               model.Description.ValueString(),
		OrganizationSelectionType: model.OrganizationSelectionType.ValueString(),
		GroupID:                   model.GroupID.ValueString(),
		NotificationSetting:       model.NotificationSetting.ValueString(),
	}
	if includeName {
		input.Name = model.Name.ValueString()
	}
	return input
}

// enterpriseTeamToModel copies the API result fields into model.
func enterpriseTeamToModel(model *EnterpriseTeamModel, team githubclient.EnterpriseTeam) {
	model.ID = types.StringValue(team.ID)
	model.Slug = types.StringValue(team.Slug)
	model.Name = types.StringValue(team.Name)
	model.Description = types.StringValue(team.Description)
	model.OrganizationSelectionType = types.StringValue(team.OrganizationSelectionType)
	model.GroupID = types.StringValue(team.GroupID)
	model.NotificationSetting = types.StringValue(team.NotificationSetting)
}
