package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func accessDomainDocument(state string) string {
	return `{"data": {"id": "dom-1", "type": "access_domain", "attributes": {
		"domain": "apps.customer.com", "kind": "custom", "state": "` + state + `",
		"organization_id": "org-1"
	}}}`
}

// The register body: domain + the resolved org riding organization_id.
// The 201 renders the pending :custom row — the canonicalized name,
// the pinned four attributes, the id at the resource level.
func TestCreateAccessDomainBodyShape(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, accessDomainDocument("pending"))
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	dom, err := c.CreateAccessDomain(context.Background(), CreateAccessDomainInput{
		Domain:         "apps.customer.com",
		OrganizationID: "org-1",
	})
	if err != nil {
		t.Fatalf("CreateAccessDomain: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/api/v1/access-domains" {
		t.Errorf("request = %s %s, want POST /api/v1/access-domains", gotMethod, gotPath)
	}
	attrs := gotBody["data"].(map[string]any)["attributes"].(map[string]any)
	if attrs["domain"] != "apps.customer.com" {
		t.Errorf("domain = %v, want the config name (canonicalization is the server's)", attrs["domain"])
	}
	if attrs["organization_id"] != "org-1" {
		t.Errorf("organization_id = %v, want org-1 (the #408 argument)", attrs["organization_id"])
	}

	if dom.ID != "dom-1" || dom.Domain != "apps.customer.com" || dom.Kind != "custom" || dom.State != "pending" {
		t.Errorf("domain = %+v, want dom-1 apps.customer.com custom pending", dom)
	}
	if dom.OrganizationID != "org-1" {
		t.Errorf("organization_id = %q, want the wire's echo", dom.OrganizationID)
	}
}

func TestCreateAccessDomainOmitsUnsetOrg(t *testing.T) {
	var gotBody map[string]any
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, accessDomainDocument("pending"))
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	if _, err := c.CreateAccessDomain(context.Background(), CreateAccessDomainInput{
		Domain: "apps.customer.com",
	}); err != nil {
		t.Fatalf("CreateAccessDomain: %v", err)
	}

	attrs := gotBody["data"].(map[string]any)["attributes"].(map[string]any)
	if _, ok := attrs["organization_id"]; ok {
		t.Error("organization_id sent — empty means the server's personal-org default")
	}
}

// The duplicate register's 400 keeps the server's wording — "is
// already registered" — in a typed error.
func TestCreateAccessDomainDuplicateIsTyped(t *testing.T) {
	srv := testServer(t, http.StatusBadRequest, `{"errors": [{"detail": "is already registered"}]}`)
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	_, err := c.CreateAccessDomain(context.Background(), CreateAccessDomainInput{Domain: "apps.customer.com"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("err = %v, want a 400 *APIError", err)
	}
	if !strings.Contains(err.Error(), "is already registered") {
		t.Errorf("Error() = %q, want the server's wording verbatim", err.Error())
	}
}

func TestGetAccessDomainParses(t *testing.T) {
	srv := testServer(t, http.StatusOK, accessDomainDocument("verified"))
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	dom, err := c.GetAccessDomain(context.Background(), "dom-1")
	if err != nil {
		t.Fatalf("GetAccessDomain: %v", err)
	}
	if dom.Kind != "custom" || dom.State != "verified" || dom.OrganizationID != "org-1" {
		t.Errorf("domain = %+v, want the parsed fields", dom)
	}
}

func TestGetAccessDomain404IsTyped(t *testing.T) {
	srv := testServer(t, http.StatusNotFound, `{"errors": [{"status": "404"}]}`)
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	_, err := c.GetAccessDomain(context.Background(), "dom-1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("err = %v, want a 404 *APIError (the removed-from-state signal)", err)
	}
}

// A missing or wrong-typed public attribute is shape drift, not a
// silent zero — the pinned field set is the contract.
func TestAccessDomainShapeDriftIsAnError(t *testing.T) {
	missingKind := `{"data": {"id": "dom-1", "type": "access_domain", "attributes": {
		"domain": "apps.customer.com", "state": "pending", "organization_id": "org-1"}}}`
	srv := testServer(t, http.StatusOK, missingKind)
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	if _, err := c.GetAccessDomain(context.Background(), "dom-1"); err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("err = %v, want the kind shape-drift error", err)
	}

	wrongTypedState := `{"data": {"id": "dom-1", "type": "access_domain", "attributes": {
		"domain": "apps.customer.com", "kind": "custom", "state": 7, "organization_id": "org-1"}}}`
	srv2 := testServer(t, http.StatusOK, wrongTypedState)
	c2 := New(srv2.URL, "fvpat_test", "test", srv2.Client())

	if _, err := c2.GetAccessDomain(context.Background(), "dom-1"); err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("err = %v, want the state shape-drift error", err)
	}
}

// The teardown DELETE: the 200 renders the row flat (the generic
// action's shape — no JSON:API envelope), which decodes harmlessly.
func TestDeleteAccessDomainFlatBody(t *testing.T) {
	var gotPath, gotMethod string
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		fmt.Fprint(w, `{"id": "dom-1", "domain": "apps.customer.com", "kind": "custom", "state": "pending"}`)
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	if err := c.DeleteAccessDomain(context.Background(), "dom-1"); err != nil {
		t.Fatalf("DeleteAccessDomain: %v", err)
	}

	if gotMethod != http.MethodDelete || gotPath != "/api/v1/access-domains/dom-1" {
		t.Errorf("request = %s %s, want DELETE /api/v1/access-domains/dom-1", gotMethod, gotPath)
	}
}
