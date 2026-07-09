// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
)

// NewInstallationTokenDataSource is a placeholder until the full implementation
// is added in datasource_installation_token.go.
func NewInstallationTokenDataSource() datasource.DataSource {
	return &installationTokenDataSource{}
}

type installationTokenDataSource struct{}

func (d *installationTokenDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_installation_token"
}

func (d *installationTokenDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
}

func (d *installationTokenDataSource) Read(_ context.Context, _ datasource.ReadRequest, _ *datasource.ReadResponse) {
}
