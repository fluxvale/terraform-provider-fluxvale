package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// Organization is one row of GET /api/v1/organizations — the actor's
// own orgs (organizations:read; membership IS the policy underneath).
// The wire carries slug + name per row, the id at the resource level
// (organization_route_test.exs). The same shape answers the detail
// reads and the create/update writes (#784).
type Organization struct {
	ID   string
	Slug string
	Name string
}

// organizationFromResource maps one wire row, with shape-drift context.
func organizationFromResource(row Resource, op string) (Organization, error) {
	if row.ID == "" {
		return Organization{}, fmt.Errorf("%s returned a row with no id — API shape drift", op)
	}
	slug, err := attrString(row.Attributes, "slug")
	if err != nil {
		return Organization{}, shapeDrift(op, row.ID, err)
	}
	name, err := attrString(row.Attributes, "name")
	if err != nil {
		return Organization{}, shapeDrift(op, row.ID, err)
	}
	return Organization{ID: row.ID, Slug: slug, Name: name}, nil
}

// ListOrganizations calls GET /api/v1/organizations.
func (c *Client) ListOrganizations(ctx context.Context) ([]Organization, error) {
	doc, err := c.DoList(ctx, http.MethodGet, "/api/v1/organizations", nil)
	if err != nil {
		return nil, err
	}

	orgs := make([]Organization, 0, len(doc.Data))
	for _, row := range doc.Data {
		org, err := organizationFromResource(row, "GET /api/v1/organizations")
		if err != nil {
			return nil, err
		}
		orgs = append(orgs, org)
	}
	return orgs, nil
}

// CreateOrganizationInput is the POST /api/v1/organizations body.
type CreateOrganizationInput struct {
	Name string
	Slug string
}

// CreateOrganization calls POST /api/v1/organizations (#784) — the
// actor path: the authenticated user becomes the team org's owner in
// the same transaction. Reserved slugs are the server's 400; the 10-org
// cap is its 403.
func (c *Client) CreateOrganization(ctx context.Context, in CreateOrganizationInput) (Organization, error) {
	path := "/api/v1/organizations"
	doc, err := c.Do(ctx, http.MethodPost, path, map[string]any{
		"data": map[string]any{
			"type":       "organization",
			"attributes": map[string]any{"name": in.Name, "slug": in.Slug},
		},
	})
	if err != nil {
		return Organization{}, err
	}
	return organizationFromResource(doc.Data, "POST "+path)
}

// GetOrganization calls GET /api/v1/organizations/:id.
func (c *Client) GetOrganization(ctx context.Context, id string) (Organization, error) {
	path := "/api/v1/organizations/" + url.PathEscape(id)
	doc, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return Organization{}, err
	}
	return organizationFromResource(doc.Data, "GET "+path)
}

// GetOrganizationBySlug calls GET /api/v1/organizations/by-slug/:slug
// — the provider's import key.
func (c *Client) GetOrganizationBySlug(ctx context.Context, slug string) (Organization, error) {
	path := "/api/v1/organizations/by-slug/" + url.PathEscape(slug)
	doc, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return Organization{}, err
	}
	return organizationFromResource(doc.Data, "GET "+path)
}

// UpdateOrganization PATCHes /api/v1/organizations/:id — the rename
// (name only; the slug is create-only in the provider).
func (c *Client) UpdateOrganization(ctx context.Context, id, name string) (Organization, error) {
	path := "/api/v1/organizations/" + url.PathEscape(id)
	doc, err := c.Do(ctx, http.MethodPatch, path, map[string]any{
		"data": map[string]any{
			"type":       "organization",
			"id":         id,
			"attributes": map[string]any{"name": name},
		},
	})
	if err != nil {
		return Organization{}, err
	}
	return organizationFromResource(doc.Data, "PATCH "+path)
}

// DeleteOrganization calls DELETE /api/v1/organizations/:id. The
// empty-org guard answers 400 with the platform's message; a 404 is
// already gone (the caller tolerates it). The 2xx body is ignored:
// the route returns the destroyed row flat, and a future bodiless
// response must not fail the delete.
func (c *Client) DeleteOrganization(ctx context.Context, id string) error {
	_, err := c.send(ctx, http.MethodDelete, "/api/v1/organizations/"+url.PathEscape(id), nil)
	return err
}
