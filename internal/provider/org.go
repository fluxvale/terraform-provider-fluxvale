package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/fluxvale/fluxvale/apps/tfprovider/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// resolveOrgID turns the resource's organization override (or the
// provider-level default) into an org id — falling back to the
// actor's sole organization. Pure lookup: the server member-fences
// the named org, so a non-member naming an org is the server's 403.
//
// Ambiguity is an error listing the orgs — never a silent pick.
func resolveOrgID(ctx context.Context, c *client.Client, override, providerDefault types.String, diags *diag.Diagnostics) string {
	spec := override
	if spec.IsNull() || spec.IsUnknown() {
		spec = providerDefault
	}

	orgs, err := c.ListOrganizations(ctx)
	if err != nil {
		diags.AddError("Unable to list your organizations", err.Error())
		return ""
	}

	if !spec.IsNull() && !spec.IsUnknown() {
		name := spec.ValueString()
		for _, org := range orgs {
			if org.ID == name || org.Slug == name {
				return org.ID
			}
		}
		diags.AddError(
			"Organization not found among your memberships",
			fmt.Sprintf("No organization with id or slug %q. Yours: %s.",
				name, orgList(orgs)),
		)
		return ""
	}

	switch len(orgs) {
	case 0:
		diags.AddError(
			"No organization to deploy into",
			"The token belongs to no organization. Set the resource's or the provider's \"organization\" once you have one.",
		)
	case 1:
		return orgs[0].ID
	default:
		diags.AddError(
			"Multiple organizations — set \"organization\"",
			fmt.Sprintf("The token belongs to %d organizations: %s. Name one via the resource's or the provider's \"organization\" attribute (id or slug).",
				len(orgs), orgList(orgs)),
		)
	}
	return ""
}

// orgList renders orgs for diagnostics: Acme (acme), B (b).
func orgList(orgs []client.Organization) string {
	parts := make([]string, 0, len(orgs))
	for _, org := range orgs {
		parts = append(parts, fmt.Sprintf("%s (%s)", org.Name, org.Slug))
	}
	return strings.Join(parts, ", ")
}
