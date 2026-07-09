// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/kreemer/terraform-provider-ghentapi/internal/githubclient"
)

var _ resource.Resource = &EnterpriseOrgResource{}
var _ resource.ResourceWithImportState = &EnterpriseOrgResource{}

// NewEnterpriseOrgResource returns a new instance of EnterpriseOrgResource.
func NewEnterpriseOrgResource() resource.Resource {
	return &EnterpriseOrgResource{}
}

// EnterpriseOrgResource manages an organisation inside a GitHub Enterprise account.
type EnterpriseOrgResource struct {
	client *githubclient.Client
}

// EnterpriseOrgModel is the Terraform state model for ghentapi_enterprise_org.
type EnterpriseOrgModel struct {
	Name         types.String `tfsdk:"name"`
	AdminLogins  types.List   `tfsdk:"admin_logins"`
	BillingEmail types.String `tfsdk:"billing_email"`
	DisplayName  types.String `tfsdk:"display_name"`
	NodeID       types.String `tfsdk:"node_id"`
}

func (r *EnterpriseOrgResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_enterprise_org"
}

func (r *EnterpriseOrgResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Creates and manages a GitHub organisation within an enterprise account. " +
			"Organisation creation uses the GraphQL `createEnterpriseOrganization` mutation authenticated " +
			"as the enterprise app. After creation the org-level app is installed automatically so that " +
			"subsequent reads and updates can authenticate with it.\n\n" +
			"**Import:** `terraform import ghentapi_enterprise_org.example my-org-login`. " +
			"During import, `EnsureOrgInstallation` is called: if the org app is not yet installed " +
			"and `auto_install_org_app = true`, it will be installed at this point. " +
			"After import, `admin_logins` will be empty in state; run `terraform apply` once to settle it " +
			"to the value in your configuration.\n\n" +
			"> **Note:** GitHub provides no API to delete organisations. When this resource is destroyed, " +
			"the organisation is only removed from Terraform state; it remains on GitHub.",

		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The organisation's login (URL slug). Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"admin_logins": schema.ListAttribute{
				Required:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Usernames of the initial organisation owners/admins.",
			},
			"billing_email": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Billing e-mail address for the organisation.",
			},
			"display_name": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The display name of the organisation (shown in the GitHub UI).",
			},
			"node_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The GraphQL node ID of the organisation.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *EnterpriseOrgResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *EnterpriseOrgResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan EnterpriseOrgModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	adminLogins := make([]string, 0, len(plan.AdminLogins.Elements()))
	resp.Diagnostics.Append(plan.AdminLogins.ElementsAs(ctx, &adminLogins, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	input := githubclient.EnterpriseOrgInput{
		Login:        plan.Name.ValueString(),
		BillingEmail: plan.BillingEmail.ValueString(),
		AdminLogins:  adminLogins,
		DisplayName:  plan.DisplayName.ValueString(),
	}

	result, err := r.client.CreateEnterpriseOrg(ctx, input)
	if err != nil {
		resp.Diagnostics.AddError("Error creating enterprise organisation", err.Error())
		return
	}

	plan.NodeID = types.StringValue(result.NodeID)
	plan.Name = types.StringValue(result.Login)

	// Explicitly install the org app into the newly created organisation so that
	// subsequent Read/Update calls can authenticate with the org app token.
	// This is distinct from the silent auto-install that happens in EnsureOrgInstallation
	// on other resources — here we know the org was just created and needs the app.
	if _, err := r.client.EnsureOrgInstallation(ctx, result.Login); err != nil {
		resp.Diagnostics.AddError("Error installing org app into new organisation", err.Error())
		return
	}

	// Populate remaining computed fields from the API.
	r.readIntoModel(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *EnterpriseOrgResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state EnterpriseOrgModel
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

func (r *EnterpriseOrgResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan EnterpriseOrgModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	patch := map[string]any{}
	if !plan.BillingEmail.IsNull() && !plan.BillingEmail.IsUnknown() {
		patch["billing_email"] = plan.BillingEmail.ValueString()
	}
	if !plan.DisplayName.IsNull() && !plan.DisplayName.IsUnknown() {
		patch["name"] = plan.DisplayName.ValueString()
	}

	if len(patch) > 0 {
		org := plan.Name.ValueString()
		patchResp, err := r.client.DoWithOrgAuth(ctx, org, http.MethodPatch, "/orgs/"+org, patch)
		if err != nil {
			resp.Diagnostics.AddError("Error updating organisation", err.Error())
			return
		}
		defer patchResp.Body.Close()
		if patchResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(patchResp.Body)
			resp.Diagnostics.AddError(
				"Error updating organisation",
				fmt.Sprintf("PATCH /orgs/%s returned status %d: %s", org, patchResp.StatusCode, string(body)),
			)
			return
		}
	}

	r.readIntoModel(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete is intentionally a no-op: GitHub provides no API to delete organisations.
func (r *EnterpriseOrgResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state EnterpriseOrgModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	tflog.Warn(ctx, "ghentapi_enterprise_org destroy is a no-op: GitHub provides no API to delete organisations. "+
		"The organisation "+state.Name.ValueString()+" still exists on GitHub.")
}

// ImportState imports an existing organisation into Terraform state by its login.
//
// Usage: terraform import ghentapi_enterprise_org.example my-org-login
//
// The subsequent Read call will invoke EnsureOrgInstallation, which may install
// the org app if it is not already present (when auto_install_org_app = true).
// After import, admin_logins will be empty in state; run terraform apply once
// to settle it to the value declared in your configuration.
func (r *EnterpriseOrgResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("name"), req, resp)
}

// readIntoModel calls GET /orgs/{name} with the org app token and updates model.
// Returns true if the org was not found (caller should remove from state).
func (r *EnterpriseOrgResource) readIntoModel(ctx context.Context, model *EnterpriseOrgModel, diags interface {
	HasError() bool
	AddError(string, string)
}) (removed bool) {
	org := model.Name.ValueString()
	getResp, err := r.client.DoWithOrgAuth(ctx, org, http.MethodGet, "/orgs/"+org, nil)
	if err != nil {
		diags.AddError("Error reading organisation", err.Error())
		return false
	}
	defer getResp.Body.Close()

	body, err := io.ReadAll(getResp.Body)
	if err != nil {
		diags.AddError("Error reading organisation response", err.Error())
		return false
	}

	if getResp.StatusCode == http.StatusNotFound {
		tflog.Warn(ctx, fmt.Sprintf("Organisation %q not found; removing from state.", org))
		return true
	}
	if getResp.StatusCode != http.StatusOK {
		diags.AddError(
			"Error reading organisation",
			fmt.Sprintf("GET /orgs/%s returned status %d: %s", org, getResp.StatusCode, string(body)),
		)
		return false
	}

	var apiData map[string]any
	if err := json.Unmarshal(body, &apiData); err != nil {
		diags.AddError("Error parsing organisation response", err.Error())
		return false
	}

	if v, ok := apiData["billing_email"]; ok && v != nil {
		model.BillingEmail = types.StringValue(fmt.Sprint(v))
	}
	// The API field "name" is the display name; our "name" attribute is the login.
	if v, ok := apiData["name"]; ok && v != nil {
		model.DisplayName = types.StringValue(fmt.Sprint(v))
	} else {
		model.DisplayName = types.StringValue("")
	}
	if v, ok := apiData["login"]; ok && v != nil {
		model.Name = types.StringValue(fmt.Sprint(v))
	}

	// admin_logins is not reconciled from the API to avoid requiring org-level
	// member-read permissions on the enterprise token. The state value is kept.
	if model.AdminLogins.IsNull() || model.AdminLogins.IsUnknown() {
		model.AdminLogins = types.ListValueMust(types.StringType, []attr.Value{})
	}

	return false
}
