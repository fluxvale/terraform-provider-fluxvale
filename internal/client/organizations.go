package client

import (
	"context"
	"fmt"
	"net/http"
)

// Organization is one row of GET /api/v1/organizations — the actor's
// own orgs (scopeless; membership IS the policy). The wire carries
// slug + name per row, the id at the resource level
// (organization_route_test.exs).
type Organization struct {
	ID   string
	Slug string
	Name string
}

// ListOrganizations calls GET /api/v1/organizations.
func (c *Client) ListOrganizations(ctx context.Context) ([]Organization, error) {
	doc, err := c.DoList(ctx, http.MethodGet, "/api/v1/organizations", nil)
	if err != nil {
		return nil, err
	}

	orgs := make([]Organization, 0, len(doc.Data))
	for _, row := range doc.Data {
		slug, err := attrString(row.Attributes, "slug")
		if err != nil {
			return nil, shapeDrift("GET /api/v1/organizations", row.ID, err)
		}
		name, err := attrString(row.Attributes, "name")
		if err != nil {
			return nil, shapeDrift("GET /api/v1/organizations", row.ID, err)
		}
		if row.ID == "" {
			return nil, fmt.Errorf("GET /api/v1/organizations returned a row with no id — API shape drift")
		}
		orgs = append(orgs, Organization{ID: row.ID, Slug: slug, Name: name})
	}
	return orgs, nil
}
