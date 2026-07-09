// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestOrgSettingResource_CreateRead(t *testing.T) {
	srv, _ := newMockGitHubServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_org_setting" "test" {
  organization = "my-org"
  settings = {
    billing_email = "new@example.com"
  }
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("ghentapi_org_setting.test", "organization", "my-org"),
					resource.TestCheckResourceAttr("ghentapi_org_setting.test", "settings.billing_email", "new@example.com"),
				),
			},
		},
	})
}

func TestOrgSettingResource_Update(t *testing.T) {
	srv, _ := newMockGitHubServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_org_setting" "test" {
  organization = "my-org"
  settings = {
    billing_email = "first@example.com"
  }
}`,
				Check: resource.TestCheckResourceAttr("ghentapi_org_setting.test", "settings.billing_email", "first@example.com"),
			},
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_org_setting" "test" {
  organization = "my-org"
  settings = {
    billing_email = "updated@example.com"
  }
}`,
				Check: resource.TestCheckResourceAttr("ghentapi_org_setting.test", "settings.billing_email", "updated@example.com"),
			},
		},
	})
}

func TestOrgSettingResource_ExtraFieldsIgnored(t *testing.T) {
	srv, _ := newMockGitHubServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				// Only billing_email is managed; extra_field from API must not appear in settings.
				Config: providerConfig(srv.URL) + `
resource "ghentapi_org_setting" "test" {
  organization = "my-org"
  settings = {
    billing_email = "check@example.com"
  }
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("ghentapi_org_setting.test", "settings.billing_email", "check@example.com"),
					resource.TestCheckNoResourceAttr("ghentapi_org_setting.test", "settings.extra_field"),
				),
			},
		},
	})
}

func TestOrgSettingResource_DriftDetection(t *testing.T) {
	srv, state := newMockGitHubServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
resource "ghentapi_org_setting" "test" {
  organization = "my-org"
  settings = {
    billing_email = "drift@example.com"
  }
}`,
				Check: resource.TestCheckResourceAttr("ghentapi_org_setting.test", "settings.billing_email", "drift@example.com"),
			},
			{
				// Simulate out-of-band change then expect a non-empty plan.
				PreConfig: func() {
					state.BillingEmail = "changed-outside-terraform@example.com"
				},
				Config: providerConfig(srv.URL) + `
resource "ghentapi_org_setting" "test" {
  organization = "my-org"
  settings = {
    billing_email = "drift@example.com"
  }
}`,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestInstallationTokenDataSource_Read(t *testing.T) {
	srv, _ := newMockGitHubServer(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: unitTestFactories(),
		Steps: []resource.TestStep{
			{
				Config: providerConfig(srv.URL) + `
data "ghentapi_installation_token" "test" {
  organization = "my-org"
}`,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrSet("data.ghentapi_installation_token.test", "token"),
					resource.TestCheckResourceAttr("data.ghentapi_installation_token.test", "organization", "my-org"),
				),
			},
		},
	})
}

// TestInstallationTokenDataSource_SensitiveAttribute verifies the token schema
// attribute is marked Sensitive without requiring a running provider.
func TestInstallationTokenDataSource_SensitiveAttribute(t *testing.T) {
	ds := &installationTokenDataSource{}
	var resp datasource.SchemaResponse
	ds.Schema(context.Background(), datasource.SchemaRequest{}, &resp)

	tokenAttr, ok := resp.Schema.Attributes["token"]
	if !ok {
		t.Fatal("token attribute not found in schema")
	}
	if !tokenAttr.IsSensitive() {
		t.Error("token attribute must be marked Sensitive")
	}
}
