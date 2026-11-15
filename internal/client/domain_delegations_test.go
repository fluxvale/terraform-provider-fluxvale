package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestGetDomainDelegationDecodesTheBareObject(t *testing.T) {
	var gotMethod, gotPath string
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		fmt.Fprint(w, `{"domain": "apps.customer.com",
			"dns_records": [{"name": "_acme-challenge.apps.customer.com", "type": "CNAME",
				"value": "0123456789abcdef0123456789abcdef.acme.fluxvale.app"}]}`)
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	got, err := c.GetDomainDelegation(context.Background(), "apps.customer.com")
	if err != nil {
		t.Fatalf("GetDomainDelegation: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/api/v1/domain-delegations/apps.customer.com" {
		t.Errorf("request = %s %s, want GET /api/v1/domain-delegations/apps.customer.com", gotMethod, gotPath)
	}
	if got.Domain != "apps.customer.com" || len(got.DNSRecords) != 1 {
		t.Fatalf("delegation = %+v, want one record", got)
	}
	if got.DNSRecords[0].Name != "_acme-challenge.apps.customer.com" ||
		got.DNSRecords[0].Type != "CNAME" ||
		got.DNSRecords[0].Value != "0123456789abcdef0123456789abcdef.acme.fluxvale.app" {
		t.Errorf("record = %+v, want the challenge CNAME", got.DNSRecords[0])
	}
}

func TestGetDomainDelegationSurfacesARefusal(t *testing.T) {
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"errors": [{"title": "InvalidArgument", "detail": "domain is not a valid domain name"}]}`)
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	_, err := c.GetDomainDelegation(context.Background(), "not a domain")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "not a valid domain") {
		t.Errorf("error = %q, want the server's detail", err)
	}
}

func TestSetGatewayApiTokenSendsThenReadsBack(t *testing.T) {
	var calls []string
	var gotBody map[string]any
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPatch {
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			// The real PATCH route answers only the claim state — no
			// tailnet_domain/provider. The client must not need them here.
			fmt.Fprint(w, `{"data": {"id": "gw-1", "type": "access_gateway", "attributes": {
				"organization_id": "org-1", "hostname": "fv-0a1b2c3d", "state": "active",
				"status_message": null, "monthly_cents": 500, "api_token_set": true,
				"token_set_at": "2026-11-15T10:00:00Z", "claimed_domains": ["apps.customer.com"]}}}`)
			return
		}
		fmt.Fprint(w, `{"data": {"id": "gw-1", "type": "access_gateway", "attributes": {
			"organization_id": "org-1", "hostname": "fv-0a1b2c3d", "tailnet_domain": "example.ts.net",
			"provider": "tailscale", "state": "active", "status_message": null,
			"monthly_cents": 500, "provisioned_at": null, "api_token_set": true,
			"token_set_at": "2026-11-15T10:00:00Z", "claimed_domains": ["apps.customer.com"]}}}`)
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	got, err := c.SetGatewayApiToken(context.Background(), "org-1", "gw-1", "tskey-api-secret")
	if err != nil {
		t.Fatalf("SetGatewayApiToken: %v", err)
	}
	wantCalls := []string{
		"PATCH /api/v1/access-gateways/gw-1/api-token",
		"GET /api/v1/organizations/org-1/access-gateway",
	}
	if strings.Join(calls, ",") != strings.Join(wantCalls, ",") {
		t.Errorf("calls = %v, want %v", calls, wantCalls)
	}
	attrs := gotBody["data"].(map[string]any)["attributes"].(map[string]any)
	if attrs["api_token"] != "tskey-api-secret" {
		t.Errorf("body api_token = %v, want the token", attrs["api_token"])
	}
	if got.TokenSetAt == nil || *got.TokenSetAt != "2026-11-15T10:00:00Z" {
		t.Errorf("token_set_at = %v, want the wire's stamp", got.TokenSetAt)
	}
	if len(got.ClaimedDomains) != 1 || got.ClaimedDomains[0] != "apps.customer.com" {
		t.Errorf("claimed_domains = %v, want [apps.customer.com]", got.ClaimedDomains)
	}
	if got.TailnetDomain != "example.ts.net" {
		t.Errorf("tailnet_domain = %q, want the full row from the read-back", got.TailnetDomain)
	}
	if got.APITokenSet == nil || !*got.APITokenSet {
		t.Error("api_token_set = false, want true")
	}
}

func TestSetGatewayApiTokenSurfacesARefusal(t *testing.T) {
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"errors": [{"title": "Forbidden", "detail": "only the organization owner can save the token"}]}`)
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	_, err := c.SetGatewayApiToken(context.Background(), "org-1", "gw-1", "tskey-api-secret")
	if err == nil {
		t.Fatal("expected the 403")
	}
	if !strings.Contains(err.Error(), "organization owner") {
		t.Errorf("error = %q, want the server's detail", err)
	}
}

func TestGetDomainDelegationRejectsMalformedJSON(t *testing.T) {
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"domain": `)
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	_, err := c.GetDomainDelegation(context.Background(), "apps.customer.com")
	if err == nil || !strings.Contains(err.Error(), "decoding response") {
		t.Fatalf("error = %v, want a decode failure", err)
	}
}

func TestGetDomainDelegationRejectsAnEmptyDomain(t *testing.T) {
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"dns_records": []}`)
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	_, err := c.GetDomainDelegation(context.Background(), "apps.customer.com")
	if err == nil || !strings.Contains(err.Error(), "without a domain") {
		t.Fatalf("error = %v, want a missing-domain refusal", err)
	}
}

func TestAttrOptStringListShapes(t *testing.T) {
	attrs := map[string]any{"claim": []any{"a", "b"}}
	got, err := attrOptStringList(attrs, "claim")
	if err != nil || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("attrOptStringList = %v, %v; want [a b]", got, err)
	}

	if got, err := attrOptStringList(attrs, "absent"); err != nil || got != nil {
		t.Fatalf("absent = %v, %v; want nil, nil", got, err)
	}

	if _, err := attrOptStringList(map[string]any{"claim": "not-a-list"}, "claim"); err == nil {
		t.Fatal("a string claim was accepted as a list")
	}

	if _, err := attrOptStringList(map[string]any{"claim": []any{"a", 7}}, "claim"); err == nil {
		t.Fatal("a non-string element was accepted")
	}
}
