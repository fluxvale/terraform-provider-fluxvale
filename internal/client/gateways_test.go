package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func gatewayDocument(state string) string {
	return `{"data": {"id": "gw-1", "type": "access_gateway", "attributes": {
		"organization_id": "org-1", "hostname": "fv-0a1b2c3d", "tailnet_domain": "example.ts.net",
		"provider": "tailscale", "state": "` + state + `", "status_message": null,
		"monthly_cents": 500, "provisioned_at": null
	}}}`
}

// gatewayCreateDocument is the CREATE response shape: provisioned_at
// is NOT in the create route's field list (only the get route carries
// it) — the fixture omits it so the parser's absence-tolerance is
// exercised for real.
func gatewayCreateDocument() string {
	return `{"data": {"id": "gw-1", "type": "access_gateway", "attributes": {
		"organization_id": "org-1", "hostname": "fv-0a1b2c3d", "tailnet_domain": "example.ts.net",
		"provider": "tailscale", "state": "provisioning", "status_message": null,
		"monthly_cents": 500
	}}}`
}

// The create body: tailnet_domain + auth_key (write-only) + the
// resolved org riding organization_id. NO monthly_cents — the create
// pins the platform's config price; a wire caller must not price its
// own gateway. The create response omits provisioned_at (not in the
// create route's field list) and must still parse.
func TestCreateGatewayBodyShape(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, gatewayCreateDocument())
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	gw, err := c.CreateGateway(context.Background(), CreateGatewayInput{
		OrganizationID: "org-1",
		TailnetDomain:  "example.ts.net",
		AuthKey:        "tskey-test",
	})
	if err != nil {
		t.Fatalf("CreateGateway: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/api/v1/access-gateways" {
		t.Errorf("request = %s %s, want POST /api/v1/access-gateways", gotMethod, gotPath)
	}
	data, _ := gotBody["data"].(map[string]any)
	attrs := data["attributes"].(map[string]any)
	if attrs["tailnet_domain"] != "example.ts.net" || attrs["auth_key"] != "tskey-test" {
		t.Errorf("attributes = %v, want tailnet_domain + auth_key", attrs)
	}
	if attrs["organization_id"] != "org-1" {
		t.Errorf("organization_id = %v, want org-1 (the #408 argument)", attrs["organization_id"])
	}
	if _, ok := attrs["monthly_cents"]; ok {
		t.Error("monthly_cents sent — the create pins the config price")
	}

	if gw.ID != "gw-1" || gw.State != "provisioning" || gw.MonthlyCents != 500 {
		t.Errorf("gateway = %+v, want gw-1 provisioning at 500", gw)
	}
	if gw.ProvisionedAt != nil || gw.StatusMessage != nil {
		t.Errorf("provisioned_at/status_message = %v/%v, want nil (null on the wire)", gw.ProvisionedAt, gw.StatusMessage)
	}
}

func TestCreateGatewayOmitsUnsetOrg(t *testing.T) {
	var gotBody map[string]any
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, gatewayCreateDocument())
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	if _, err := c.CreateGateway(context.Background(), CreateGatewayInput{
		TailnetDomain: "example.ts.net", AuthKey: "tskey-test",
	}); err != nil {
		t.Fatalf("CreateGateway: %v", err)
	}

	attrs := gotBody["data"].(map[string]any)["attributes"].(map[string]any)
	if _, ok := attrs["organization_id"]; ok {
		t.Error("organization_id sent — empty means the server's personal-org default")
	}
}

func TestGetOrgGateway404IsTyped(t *testing.T) {
	srv := testServer(t, http.StatusNotFound, `{"errors": [{"status": "404"}]}`)
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	_, err := c.GetOrgGateway(context.Background(), "org-1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("err = %v, want a 404 *APIError (the removed-from-state signal)", err)
	}
}

func TestGetOrgGatewayParses(t *testing.T) {
	srv := testServer(t, http.StatusOK, gatewayDocument("active"))
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	gw, err := c.GetOrgGateway(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("GetOrgGateway: %v", err)
	}
	if gw.Provider != "tailscale" || gw.TailnetDomain != "example.ts.net" {
		t.Errorf("gateway = %+v, want the parsed fields", gw)
	}
}

// The pairing verbs: instance_id sits at the DATA level (the generic
// action's shape, not an attributes object), and the flat/flag bodies
// decode harmlessly.
func TestAttachInstanceBodyShape(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		// The 201 renders the flat pairing row (route test's shape).
		fmt.Fprint(w, `{"gateway_id": "gw-1", "instance_id": "inst-a", "route_key": "8443"}`)
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	if err := c.AttachGatewayInstance(context.Background(), "gw-1", "inst-a"); err != nil {
		t.Fatalf("AttachGatewayInstance: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/api/v1/access-gateways/gw-1/attachments" {
		t.Errorf("request = %s %s, want POST to the attachments route", gotMethod, gotPath)
	}
	data, ok := gotBody["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %+v, want the generic action's data object", gotBody)
	}
	if data["instance_id"] != "inst-a" {
		t.Errorf("data.instance_id = %v, want inst-a at the data level", data["instance_id"])
	}
	if _, ok := data["attributes"]; ok {
		t.Error("attributes object sent — the attach action takes instance_id at the data level")
	}
}

func TestDetachInstancePathAndFlatBody(t *testing.T) {
	var gotPath, gotMethod string
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		fmt.Fprint(w, `{"success": true}`)
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	if err := c.DetachGatewayInstance(context.Background(), "gw-1", "inst-b"); err != nil {
		t.Fatalf("DetachGatewayInstance: %v", err)
	}

	if gotMethod != http.MethodDelete || gotPath != "/api/v1/access-gateways/gw-1/attachments/inst-b" {
		t.Errorf("request = %s %s, want DELETE /attachments/inst-b", gotMethod, gotPath)
	}
}

// A detach 404 is already-gone — the platform re-reads the instance
// first, so an instance deleted between refresh and apply 404s. The
// verb answers nil; an attach 404 stays a real error.
func TestDetachGatewayInstanceTolerates404(t *testing.T) {
	srv := testServer(t, http.StatusNotFound, `{"errors": [{"status": "404"}]}`)
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	if err := c.DetachGatewayInstance(context.Background(), "gw-1", "gone"); err != nil {
		t.Fatalf("DetachGatewayInstance over a 404: %v, want nil (already-gone)", err)
	}
}
