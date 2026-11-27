package client

import (
	"context"
	"net/http"
)

// InstanceHostname is one address an instance answers on, under an
// access domain: a label below the domain's name. The wire surface is
// its identity and where it sits — id, label, instance_id,
// access_domain_id — and nothing else (instance_hostname.ex's pinned
// field set). Rows are immutable on the member API (no update route);
// the mint's POST, the instance's GET list, and the DELETE-by-id are
// the whole surface.
type InstanceHostname struct {
	ID             string
	Label          string
	InstanceID     string
	AccessDomainID string
}

func instanceHostnameFromResource(row Resource) (InstanceHostname, error) {
	label, err := attrString(row.Attributes, "label")
	if err != nil {
		return InstanceHostname{}, err
	}
	instanceID, err := attrString(row.Attributes, "instance_id")
	if err != nil {
		return InstanceHostname{}, err
	}
	domainID, err := attrString(row.Attributes, "access_domain_id")
	if err != nil {
		return InstanceHostname{}, err
	}
	return InstanceHostname{
		ID:             row.ID,
		Label:          label,
		InstanceID:     instanceID,
		AccessDomainID: domainID,
	}, nil
}

// MintInstanceHostnameInput is the POST
// /api/v1/instances/:instance_id/hostnames body.
type MintInstanceHostnameInput struct {
	InstanceID     string
	AccessDomainID string
	Label          string
}

// MintInstanceHostname calls POST
// /api/v1/instances/:instance_id/hostnames — the mint. instance_id
// rides the path, never the body. Refusals pass through typed, the
// server's wording verbatim: 403 without the instances:write scope;
// 404 for a foreign instance or a domain the actor cannot read (no
// row lands); 400 "that domain belongs to a different organization";
// 400 "only a custom domain can host a custom address"; 400 "verify
// the domain before adding addresses to it"; 400 or 409 for a
// duplicate (access_domain_id, label) — the row already stands; and
// the two partial-success 400s — "The address was added, but the app
// could not be restarted to pick it up. It will pick it up on its
// next change or a redeploy." and "The address was added, but the app
// could not be updated to use it. It will pick it up on a redeploy."
// — where the row stands. The 404 fences leave no row; the two "The
// address was added" 400s leave the row standing — the resource layer
// probes for that.
func (c *Client) MintInstanceHostname(ctx context.Context, in MintInstanceHostnameInput) (InstanceHostname, error) {
	doc, err := c.Do(ctx, http.MethodPost, "/api/v1/instances/"+in.InstanceID+"/hostnames", map[string]any{
		"data": map[string]any{
			"type": "instance_hostname",
			"attributes": map[string]any{
				"access_domain_id": in.AccessDomainID,
				"label":            in.Label,
			},
		},
	})
	if err != nil {
		return InstanceHostname{}, err
	}
	hostname, err := instanceHostnameFromResource(doc.Data)
	if err != nil {
		return InstanceHostname{}, shapeDrift("POST /api/v1/instances/:id/hostnames", doc.Data.ID, err)
	}
	return hostname, nil
}

// ListInstanceHostnames calls GET
// /api/v1/instances/:instance_id/hostnames — the instance's
// addresses, oldest first, the platform-minted default row (label =
// the instance slug) included. Index semantics: a foreign or gone
// instance answers 200 with an empty collection, never a
// distinguishable 404 — the caller's "gone" signal is row-absence,
// not status.
func (c *Client) ListInstanceHostnames(ctx context.Context, instanceID string) ([]InstanceHostname, error) {
	doc, err := c.DoList(ctx, http.MethodGet, "/api/v1/instances/"+instanceID+"/hostnames", nil)
	if err != nil {
		return nil, err
	}
	hostnames := make([]InstanceHostname, 0, len(doc.Data))
	for _, row := range doc.Data {
		hostname, err := instanceHostnameFromResource(row)
		if err != nil {
			return nil, shapeDrift("GET /api/v1/instances/:id/hostnames", row.ID, err)
		}
		hostnames = append(hostnames, hostname)
	}
	return hostnames, nil
}

// RemoveInstanceHostname calls DELETE /api/v1/instance-hostnames/:id.
// The 200 renders the removed row flat (a bare JSON object — no
// JSON:API envelope), which the Document decode absorbs harmlessly
// (the same stance as DeleteAccessDomain). A gone row answers 404,
// the typed *APIError — the caller's already-gone signal.
func (c *Client) RemoveInstanceHostname(ctx context.Context, hostnameID string) error {
	_, err := c.Do(ctx, http.MethodDelete, "/api/v1/instance-hostnames/"+hostnameID, nil)
	return err
}
