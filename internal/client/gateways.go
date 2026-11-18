package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// AccessGateway is the org's singleton gateway — GET
// /api/v1/organizations/:organization_id/access-gateway and the POST
// create render the same fields (access_gateway.ex default_fields).
// The credential refs are private by design (never on the wire) and
// auth_key is a write-only create argument that never comes back.
type AccessGateway struct {
	ID             string
	OrganizationID string
	Hostname       string
	TailnetDomain  string
	Provider       string
	State          string
	StatusMessage  *string
	MonthlyCents   int64
	ProvisionedAt  *string
	// The claim path's readable half (fluxvale/fluxvale#771, fluxvale/fluxvale#783): whether the token
	// intake has landed, the rotation clock, and the claimed domain
	// names. The token value itself never comes back.
	APITokenSet    *bool
	TokenSetAt     *string
	ClaimedDomains []string
}

func accessGatewayFromResource(row Resource) (AccessGateway, error) {
	orgID, err := attrString(row.Attributes, "organization_id")
	if err != nil {
		return AccessGateway{}, err
	}
	hostname, err := attrString(row.Attributes, "hostname")
	if err != nil {
		return AccessGateway{}, err
	}
	domain, err := attrString(row.Attributes, "tailnet_domain")
	if err != nil {
		return AccessGateway{}, err
	}
	provider, err := attrString(row.Attributes, "provider")
	if err != nil {
		return AccessGateway{}, err
	}
	state, err := attrString(row.Attributes, "state")
	if err != nil {
		return AccessGateway{}, err
	}
	cents, err := attrOptInt64(row.Attributes, "monthly_cents")
	if err != nil {
		return AccessGateway{}, err
	}
	if cents == nil {
		return AccessGateway{}, fmt.Errorf("monthly_cents is missing or null")
	}
	statusMessage, err := attrOptString(row.Attributes, "status_message")
	if err != nil {
		return AccessGateway{}, err
	}
	// The create response omits provisioned_at (not in its field
	// list) — absent, not drift.
	provisionedAt, err := attrOptString(row.Attributes, "provisioned_at")
	if err != nil {
		return AccessGateway{}, err
	}
	tokenSet, err := attrOptBool(row.Attributes, "api_token_set")
	if err != nil {
		return AccessGateway{}, err
	}
	tokenSetAt, err := attrOptString(row.Attributes, "token_set_at")
	if err != nil {
		return AccessGateway{}, err
	}
	claimedDomains, err := attrOptStringList(row.Attributes, "claimed_domains")
	if err != nil {
		return AccessGateway{}, err
	}
	return AccessGateway{
		ID:             row.ID,
		OrganizationID: orgID,
		Hostname:       hostname,
		TailnetDomain:  domain,
		Provider:       provider,
		State:          state,
		StatusMessage:  statusMessage,
		MonthlyCents:   *cents,
		ProvisionedAt:  provisionedAt,
		APITokenSet:    tokenSet,
		TokenSetAt:     tokenSetAt,
		ClaimedDomains: claimedDomains,
	}, nil
}

// SetGatewayApiToken calls PATCH /api/v1/access-gateways/:id/api-token
// (fluxvale/fluxvale#760) — the same intake the panel uses. The token is write-only: it
// rides the request and the response never echoes it. The PATCH route
// answers only the claim state, not the full row, so the singleton read
// follows to return what mapGateway expects. The save claims the org's
// already-verified domains; the read carries the result. Owner-only
// server-side; a 403 surfaces verbatim.
func (c *Client) SetGatewayApiToken(ctx context.Context, orgID, id, token string) (AccessGateway, error) {
	if _, err := c.send(ctx, http.MethodPatch, "/api/v1/access-gateways/"+id+"/api-token", map[string]any{
		"data": map[string]any{
			"type":       "access_gateway",
			"attributes": map[string]any{"api_token": token},
		},
	}); err != nil {
		return AccessGateway{}, err
	}
	return c.GetOrgGateway(ctx, orgID)
}

// SetGatewayClientPair calls PATCH /api/v1/access-gateways/:id/api-token
// with the OAuth client pair (#836 lane B) — the same intake the
// panel uses, one shape per save: the pair REPLACES a stored console
// token in the credential Secret and vice versa (the platform's
// store is an upsert of the whole data map). The client id is public
// identity; the secret is write-only — it rides the request and the
// response never echoes it. Exactly one shape per call: the pair
// beside a token (or either half alone) is a 400 whose field-scoped
// sentence surfaces verbatim. The PATCH route answers only the claim
// state, not the full row, so the singleton read follows to return
// what mapGateway expects. Owner-only server-side; a 403 surfaces
// verbatim.
func (c *Client) SetGatewayClientPair(ctx context.Context, orgID, id, clientID, clientSecret string) (AccessGateway, error) {
	if _, err := c.send(ctx, http.MethodPatch, "/api/v1/access-gateways/"+id+"/api-token", map[string]any{
		"data": map[string]any{
			"type": "access_gateway",
			"attributes": map[string]any{
				"ts_client_id":     clientID,
				"ts_client_secret": clientSecret,
			},
		},
	}); err != nil {
		return AccessGateway{}, err
	}
	return c.GetOrgGateway(ctx, orgID)
}

// CreateGatewayInput is the POST /api/v1/access-gateways body.
type CreateGatewayInput struct {
	OrganizationID string // "" omits — the server falls back to the actor's personal org
	TailnetDomain  string
	AuthKey        string // write-only: never rendered in any response — the legacy enrollment credential
	// ApiToken is write-only: omitted when empty — the platform mints
	// the join key from it (fluxvale/fluxvale#836). The wire takes
	// exactly one credential: both (or neither) of AuthKey/ApiToken is
	// a 400 whose pinned sentence surfaces verbatim.
	ApiToken string
	// TsClientID/TsClientSecret are the OAuth client pair (#836 lane
	// B) — the preferred enrollment credential. The secret is
	// write-only (never rendered in any response); the id is public
	// identity and is NOT sensitive on the wire. Both are omitted when
	// empty (the ApiToken style), and the wire still takes exactly one
	// credential shape: the pair beside auth_key or api_token is a 400
	// whose field-scoped sentence surfaces verbatim, as is a lone
	// pair half.
	TsClientID     string
	TsClientSecret string
}

// CreateGateway calls POST /api/v1/access-gateways. The create is
// synchronous server-side (it holds the enrollment credential and
// rolls back on failure) but lands the row :provisioning — the
// observer trigger is the only promoter to :active, so attaching waits
// separately. The monthly price is deliberately not sent: the create
// pins the platform's config figure — a wire caller must not price
// their own gateway.
func (c *Client) CreateGateway(ctx context.Context, in CreateGatewayInput) (AccessGateway, error) {
	attrs := map[string]any{
		"tailnet_domain": in.TailnetDomain,
		"auth_key":       in.AuthKey,
	}
	if in.OrganizationID != "" {
		attrs["organization_id"] = in.OrganizationID
	}
	if in.ApiToken != "" {
		attrs["api_token"] = in.ApiToken
	}
	if in.TsClientID != "" {
		attrs["ts_client_id"] = in.TsClientID
	}
	if in.TsClientSecret != "" {
		attrs["ts_client_secret"] = in.TsClientSecret
	}
	doc, err := c.Do(ctx, http.MethodPost, "/api/v1/access-gateways", map[string]any{
		"data": map[string]any{"type": "access_gateway", "attributes": attrs},
	})
	if err != nil {
		return AccessGateway{}, err
	}
	gw, err := accessGatewayFromResource(doc.Data)
	if err != nil {
		return AccessGateway{}, shapeDrift("POST /api/v1/access-gateways", doc.Data.ID, err)
	}
	return gw, nil
}

// GetOrgGateway calls GET /api/v1/organizations/:organization_id/access-gateway
// — the singleton read (at most one row per org). A 404 is the
// *APIError (Status 404): the org has no gateway — removed-from-state
// for the resource's Read.
func (c *Client) GetOrgGateway(ctx context.Context, orgID string) (AccessGateway, error) {
	doc, err := c.Do(ctx, http.MethodGet, "/api/v1/organizations/"+orgID+"/access-gateway", nil)
	if err != nil {
		return AccessGateway{}, err
	}
	gw, err := accessGatewayFromResource(doc.Data)
	if err != nil {
		return AccessGateway{}, shapeDrift("GET /api/v1/organizations/"+orgID+"/access-gateway", doc.Data.ID, err)
	}
	return gw, nil
}

// AttachGatewayInstance POSTs /api/v1/access-gateways/:id/attachments.
// Generic-action body shape: instance_id sits at the DATA level, not
// inside attributes (gateway_route_test.exs). The 201 renders the flat
// pairing row, which decodes as an empty Document harmlessly. The
// platform refuses non-private instances, cross-org pairings, and
// gateways that are not serving — those 400s surface verbatim.
func (c *Client) AttachGatewayInstance(ctx context.Context, gatewayID, instanceID string) error {
	_, err := c.Do(ctx, http.MethodPost, "/api/v1/access-gateways/"+gatewayID+"/attachments", map[string]any{
		"data": map[string]any{"instance_id": instanceID},
	})
	return err
}

// DetachGatewayInstance DELETEs
// /api/v1/access-gateways/:gateway_id/attachments/:instance_id —
// idempotent, like the panel's. The 200 {"success": true} decodes
// harmlessly. A 404 is already-gone, not a failure: the platform's
// detach re-reads both rows through their authorized get_by_id
// (attach_instance's exact call path), so an instance deleted between
// refresh and apply 404s — the desired end state. (Attach keeps its
// 404s real errors: there a missing row is a refusal, not
// convergence.)
func (c *Client) DetachGatewayInstance(ctx context.Context, gatewayID, instanceID string) error {
	_, err := c.Do(ctx, http.MethodDelete, "/api/v1/access-gateways/"+gatewayID+"/attachments/"+instanceID, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return nil
	}
	return err
}
