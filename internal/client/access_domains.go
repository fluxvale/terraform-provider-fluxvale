package client

import (
	"context"
	"net/http"
)

// AccessDomain is a domain registered on the private-access edge —
// POST /api/v1/access-domains (the register) and GET
// /api/v1/access-domains/:id render the same pinned public field set
// (access_domain_route_test.exs): domain, kind, state,
// organization_id, with the id at the resource level. cert_secret_ref
// is an internal k8s object name — never API surface.
type AccessDomain struct {
	ID             string
	Domain         string
	Kind           string
	State          string
	OrganizationID string
}

func accessDomainFromResource(row Resource) (AccessDomain, error) {
	domain, err := attrString(row.Attributes, "domain")
	if err != nil {
		return AccessDomain{}, err
	}
	kind, err := attrString(row.Attributes, "kind")
	if err != nil {
		return AccessDomain{}, err
	}
	state, err := attrString(row.Attributes, "state")
	if err != nil {
		return AccessDomain{}, err
	}
	orgID, err := attrString(row.Attributes, "organization_id")
	if err != nil {
		return AccessDomain{}, err
	}
	return AccessDomain{
		ID:             row.ID,
		Domain:         domain,
		Kind:           kind,
		State:          state,
		OrganizationID: orgID,
	}, nil
}

// CreateAccessDomainInput is the POST /api/v1/access-domains body.
type CreateAccessDomainInput struct {
	Domain         string
	OrganizationID string // "" omits — the server falls back to the actor's personal org
}

// CreateAccessDomain calls POST /api/v1/access-domains — the
// register. Server-side, inside the create's transaction: the name is
// canonicalized (trimmed, downcased), the row lands :pending :custom,
// and the wildcard Certificate mints (verify-by-issuance) — failure
// is a 5xx with NO row. A duplicate name is the server's 400 "is
// already registered": platform-wide uniqueness across ALL
// organizations, so the row behind the refusal may belong to another
// org — no adoption probe, the error surfaces verbatim.
func (c *Client) CreateAccessDomain(ctx context.Context, in CreateAccessDomainInput) (AccessDomain, error) {
	attrs := map[string]any{"domain": in.Domain}
	if in.OrganizationID != "" {
		attrs["organization_id"] = in.OrganizationID
	}
	doc, err := c.Do(ctx, http.MethodPost, "/api/v1/access-domains", map[string]any{
		"data": map[string]any{"type": "access_domain", "attributes": attrs},
	})
	if err != nil {
		return AccessDomain{}, err
	}
	domain, err := accessDomainFromResource(doc.Data)
	if err != nil {
		return AccessDomain{}, shapeDrift("POST /api/v1/access-domains", doc.Data.ID, err)
	}
	return domain, nil
}

// GetAccessDomain calls GET /api/v1/access-domains/:id. A 404 is the
// *APIError (Status 404) — the removed-from-state signal for the
// resource's Read.
func (c *Client) GetAccessDomain(ctx context.Context, id string) (AccessDomain, error) {
	doc, err := c.Do(ctx, http.MethodGet, "/api/v1/access-domains/"+id, nil)
	if err != nil {
		return AccessDomain{}, err
	}
	domain, err := accessDomainFromResource(doc.Data)
	if err != nil {
		return AccessDomain{}, shapeDrift("GET /api/v1/access-domains/"+id, doc.Data.ID, err)
	}
	return domain, nil
}

// DeleteAccessDomain calls DELETE /api/v1/access-domains/:id — the
// FULL teardown server-side: hostname pre-check, tailnet unclaim, row
// destroy, CoreDNS re-render, Certificate safe-delete. Synchronous —
// the caller polls nothing; a 404 is already-gone. The platform-kind
// refusal and the serving-hostnames guard are its 400s (verbatim).
// The 200 renders the row flat (the generic-action shape, no JSON:API
// envelope) which decodes as an empty Document harmlessly.
func (c *Client) DeleteAccessDomain(ctx context.Context, id string) error {
	_, err := c.Do(ctx, http.MethodDelete, "/api/v1/access-domains/"+id, nil)
	return err
}
