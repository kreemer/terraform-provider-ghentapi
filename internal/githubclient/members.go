// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package githubclient

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// OrgMember describes a single member of a GitHub organisation.
type OrgMember struct {
	Login string
	// Email is the member's publicly visible GitHub profile email, if any.
	// GitHub's API never exposes a member's private/verified email address,
	// so this may be empty even for members who have an email on file.
	Email   string
	IsOwner bool
}

const orgMembersQuery = `
query($org: String!, $after: String) {
  organization(login: $org) {
    membersWithRole(first: 100, after: $after) {
      edges {
        role
        node {
          login
          email
        }
      }
      pageInfo {
        hasNextPage
        endCursor
      }
    }
  }
}`

type orgMembersResponse struct {
	Organization struct {
		MembersWithRole struct {
			Edges []struct {
				Role string `json:"role"`
				Node struct {
					Login string `json:"login"`
					Email string `json:"email"`
				} `json:"node"`
			} `json:"edges"`
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
		} `json:"membersWithRole"`
	} `json:"organization"`
}

// ListOrganizationMembers returns all members of the given organisation,
// including their login, publicly visible email (if any), and whether they
// hold the ADMIN (owner) role. Results are sorted by login for stable output.
func (c *Client) ListOrganizationMembers(ctx context.Context, org string) ([]OrgMember, error) {
	var members []OrgMember
	after := ""

	for {
		variables := map[string]any{"org": org}
		if after != "" {
			variables["after"] = after
		} else {
			variables["after"] = nil
		}

		data, err := c.DoGraphQLWithOrgAuth(ctx, org, orgMembersQuery, variables)
		if err != nil {
			return nil, fmt.Errorf("listing members for org %q: %w", org, err)
		}

		var resp orgMembersResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("decoding members response for org %q: %w", org, err)
		}

		for _, edge := range resp.Organization.MembersWithRole.Edges {
			members = append(members, OrgMember{
				Login:   edge.Node.Login,
				Email:   edge.Node.Email,
				IsOwner: edge.Role == "ADMIN",
			})
		}

		if !resp.Organization.MembersWithRole.PageInfo.HasNextPage {
			break
		}
		after = resp.Organization.MembersWithRole.PageInfo.EndCursor
	}

	sort.Slice(members, func(i, j int) bool {
		return members[i].Login < members[j].Login
	})

	return members, nil
}
