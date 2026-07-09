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
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/kreemer/terraform-provider-ghentapi/internal/githubclient"
)

var _ resource.Resource = &orgSettingResource{}

func NewOrgSettingResource() resource.Resource {
	return &orgSettingResource{}
}

type orgSettingResource struct {
	client *githubclient.Client
}

type orgSettingModel struct {
	Organization types.String `tfsdk:"organization"`
	Settings     types.Map    `tfsdk:"settings"`
	APIResponse  types.String `tfsdk:"api_response"`
}

func (r *orgSettingResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_setting"
}

func (r *orgSettingResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a set of organisation-level settings. Only the keys present in `settings` are managed; all other org settings are left untouched.",
		Attributes: map[string]schema.Attribute{
			"organization": schema.StringAttribute{
				MarkdownDescription: "GitHub organisation login.",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"settings": schema.MapAttribute{
				MarkdownDescription: "Map of setting key → value. Only these keys are drift-checked; unmanaged fields are ignored.",
				Required:            true,
				ElementType:         types.StringType,
			},
			"api_response": schema.StringAttribute{
				MarkdownDescription: "Full JSON body from the last successful GET (for debugging).",
				Computed:            true,
				Sensitive:           true,
			},
		},
	}
}

func (r *orgSettingResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*githubclient.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data type",
			fmt.Sprintf("Expected *githubclient.Client, got %T", req.ProviderData))
		return
	}
	r.client = client
}

func (r *orgSettingResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan orgSettingModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.patchSettings(ctx, plan.Organization.ValueString(), plan.Settings); err != nil {
		resp.Diagnostics.AddError("Error patching org settings", err.Error())
		return
	}

	body, err := r.getOrg(ctx, plan.Organization.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error reading org after create", err.Error())
		return
	}

	state, diags := r.buildState(ctx, plan.Organization.ValueString(), plan.Settings, body)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *orgSettingResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state orgSettingModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := r.getOrg(ctx, state.Organization.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error reading org", err.Error())
		return
	}

	newState, diags := r.buildState(ctx, state.Organization.ValueString(), state.Settings, body)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, newState)...)
}

func (r *orgSettingResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan orgSettingModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.patchSettings(ctx, plan.Organization.ValueString(), plan.Settings); err != nil {
		resp.Diagnostics.AddError("Error patching org settings", err.Error())
		return
	}

	body, err := r.getOrg(ctx, plan.Organization.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error reading org after update", err.Error())
		return
	}

	state, diags := r.buildState(ctx, plan.Organization.ValueString(), plan.Settings, body)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *orgSettingResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
	// Org settings cannot be deleted — the resource is simply removed from state.
}

// patchSettings sends a PATCH /orgs/{org} with the managed settings map.
func (r *orgSettingResource) patchSettings(ctx context.Context, org string, settings types.Map) error {
	payload := make(map[string]string)
	for k, v := range settings.Elements() {
		sv, ok := v.(types.String)
		if !ok {
			continue
		}
		payload[k] = sv.ValueString()
	}

	resp, err := r.client.DoWithOrgAuth(ctx, org, http.MethodPatch, "/orgs/"+org, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("PATCH /orgs/%s failed (status %d): %s", org, resp.StatusCode, string(body))
	}
	return nil
}

// getOrg calls GET /orgs/{org} and returns the raw response body.
func (r *orgSettingResource) getOrg(ctx context.Context, org string) ([]byte, error) {
	resp, err := r.client.DoWithOrgAuth(ctx, org, http.MethodGet, "/orgs/"+org, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading org response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /orgs/%s failed (status %d): %s", org, resp.StatusCode, string(body))
	}
	return body, nil
}

// buildState constructs the Terraform state from the API response, keeping
// only the keys present in managedSettings.
func (r *orgSettingResource) buildState(_ context.Context, org string, managedSettings types.Map, body []byte) (*orgSettingModel, diag.Diagnostics) {
	var diags diag.Diagnostics

	var apiData map[string]interface{}
	if err := json.Unmarshal(body, &apiData); err != nil {
		diags.AddError("Error parsing org response", err.Error())
		return nil, diags
	}

	result := make(map[string]attr.Value, len(managedSettings.Elements()))
	for k := range managedSettings.Elements() {
		if v, ok := apiData[k]; ok {
			result[k] = types.StringValue(fmt.Sprint(v))
		} else {
			result[k] = types.StringValue("")
		}
	}

	settingsMap, mapDiags := types.MapValue(types.StringType, result)
	diags.Append(mapDiags...)
	if diags.HasError() {
		return nil, diags
	}

	return &orgSettingModel{
		Organization: types.StringValue(org),
		Settings:     settingsMap,
		APIResponse:  types.StringValue(string(body)),
	}, diags
}
