package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// DNSRecord is one delegation record a custom domain needs at its DNS
// host. The provider exposes it as the data source's dns_records element
// and binds it to a DNS-provider resource; the platform's private lane
// returns exactly one _acme-challenge CNAME.
type DNSRecord struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// DomainDelegation is GET /api/v1/domain-delegations/:domain — the
// pre-registration preview (fluxvale/fluxvale#783). It is deliberately row-free: the
// domain is the input, so a Terraform graph can create the DNS record
// before fluxvale_custom_domain's create waits for DNS-01 issuance.
type DomainDelegation struct {
	Domain     string      `json:"domain"`
	DNSRecords []DNSRecord `json:"dns_records"`
}

// GetDomainDelegation calls GET /api/v1/domain-delegations/:domain.
// The route answers a bare JSON object (a generic action, like
// /pricing/quote), not a JSON:API document, so this method decodes the
// raw body through the one transport path. A 400 is the server's
// validation refusal (invalid name, or FluxVale's own platform domain)
// and surfaces verbatim.
func (c *Client) GetDomainDelegation(ctx context.Context, domain string) (DomainDelegation, error) {
	path := "/api/v1/domain-delegations/" + url.PathEscape(domain)
	body, err := c.send(ctx, http.MethodGet, path, nil)
	if err != nil {
		return DomainDelegation{}, err
	}

	var delegation DomainDelegation
	if err := json.Unmarshal(body, &delegation); err != nil {
		return DomainDelegation{}, fmt.Errorf("decoding response from GET %s: %w", path, err)
	}
	if delegation.Domain == "" {
		return DomainDelegation{}, fmt.Errorf("GET %s answered without a domain", path)
	}
	return delegation, nil
}
