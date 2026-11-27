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

func instanceHostnameDocument(id, label string) string {
	return `{"data": {"id": "` + id + `", "type": "instance_hostname", "attributes": {
		"label": "` + label + `", "instance_id": "inst-1", "access_domain_id": "dom-1"
	}}}`
}

// The mint body: the label + the domain riding the attributes,
// data.type instance_hostname. instance_id never rides the body — it
// is the path's :instance_id. The 201 renders the row — the pinned
// four fields, the id at the resource level.
func TestMintInstanceHostnameBodyShape(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, instanceHostnameDocument("host-1", "git"))
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	hostname, err := c.MintInstanceHostname(context.Background(), MintInstanceHostnameInput{
		InstanceID:     "inst-1",
		AccessDomainID: "dom-1",
		Label:          "git",
	})
	if err != nil {
		t.Fatalf("MintInstanceHostname: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/api/v1/instances/inst-1/hostnames" {
		t.Errorf("request = %s %s, want POST /api/v1/instances/inst-1/hostnames", gotMethod, gotPath)
	}
	data, _ := gotBody["data"].(map[string]any)
	if data == nil || data["type"] != "instance_hostname" {
		t.Fatalf("data = %+v, want type instance_hostname", gotBody["data"])
	}
	attrs, _ := data["attributes"].(map[string]any)
	if attrs == nil {
		t.Fatal("data.attributes missing — the mint's two fields ride the attributes object")
	}
	if attrs["access_domain_id"] != "dom-1" {
		t.Errorf("attributes.access_domain_id = %v, want dom-1", attrs["access_domain_id"])
	}
	if attrs["label"] != "git" {
		t.Errorf("attributes.label = %v, want git", attrs["label"])
	}
	if _, ok := attrs["instance_id"]; ok {
		t.Error("attributes.instance_id sent — the instance rides the path, never the body")
	}

	if hostname.ID != "host-1" || hostname.Label != "git" || hostname.InstanceID != "inst-1" || hostname.AccessDomainID != "dom-1" {
		t.Errorf("hostname = %+v, want host-1 git inst-1 dom-1", hostname)
	}
}

// A refusal keeps the server's wording in a typed *APIError — the
// client's only job is to let it through verbatim.
func TestMintInstanceHostnameRefusalIsTyped(t *testing.T) {
	srv := testServer(t, http.StatusBadRequest, `{"errors": [{"detail": "only a custom domain can host a custom address"}]}`)
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	_, err := c.MintInstanceHostname(context.Background(), MintInstanceHostnameInput{
		InstanceID:     "inst-1",
		AccessDomainID: "dom-1",
		Label:          "git",
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("err = %v, want a 400 *APIError", err)
	}
	if !strings.Contains(err.Error(), "only a custom domain can host a custom address") {
		t.Errorf("Error() = %q, want the server's wording verbatim", err.Error())
	}
}

// The instance's list: the platform-minted default row (the instance
// slug as label) first, then the custom mints — oldest first on the
// wire.
func TestListInstanceHostnamesDecodes(t *testing.T) {
	var gotPath string
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprint(w, `{"data": [
			{"id": "host-0", "type": "instance_hostname", "attributes": {
				"label": "velvety-otter-1234", "instance_id": "inst-1", "access_domain_id": "dom-platform"}},
			{"id": "host-1", "type": "instance_hostname", "attributes": {
				"label": "git", "instance_id": "inst-1", "access_domain_id": "dom-1"}}]}`)
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	hostnames, err := c.ListInstanceHostnames(context.Background(), "inst-1")
	if err != nil {
		t.Fatalf("ListInstanceHostnames: %v", err)
	}

	if gotPath != "/api/v1/instances/inst-1/hostnames" {
		t.Errorf("path = %q, want /api/v1/instances/inst-1/hostnames", gotPath)
	}
	if len(hostnames) != 2 {
		t.Fatalf("rows = %d, want 2", len(hostnames))
	}
	if hostnames[0].ID != "host-0" || hostnames[0].Label != "velvety-otter-1234" || hostnames[0].InstanceID != "inst-1" || hostnames[0].AccessDomainID != "dom-platform" {
		t.Errorf("row 0 = %+v, want the default slug row", hostnames[0])
	}
	if hostnames[1].ID != "host-1" || hostnames[1].Label != "git" || hostnames[1].InstanceID != "inst-1" || hostnames[1].AccessDomainID != "dom-1" {
		t.Errorf("row 1 = %+v, want the git row", hostnames[1])
	}
}

// Index semantics: a foreign or gone instance answers 200 with an
// empty collection — row-absence is the caller's gone signal, not a
// status code.
func TestListInstanceHostnamesEmptyIsNotAnError(t *testing.T) {
	srv := testServer(t, http.StatusOK, `{"data": []}`)
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	hostnames, err := c.ListInstanceHostnames(context.Background(), "inst-foreign")
	if err != nil {
		t.Fatalf("ListInstanceHostnames: %v", err)
	}
	if len(hostnames) != 0 {
		t.Errorf("rows = %d, want an empty list", len(hostnames))
	}
}

// A list row missing a pinned attribute is shape drift, not a silent
// zero — the error names the endpoint and the field.
func TestListInstanceHostnamesShapeDriftIsAnError(t *testing.T) {
	doc := `{"data": [{"id": "host-1", "type": "instance_hostname", "attributes": {
		"instance_id": "inst-1", "access_domain_id": "dom-1"}}]}`
	srv := testServer(t, http.StatusOK, doc)
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	_, err := c.ListInstanceHostnames(context.Background(), "inst-1")
	if err == nil || !strings.Contains(err.Error(), "GET /api/v1/instances/:id/hostnames") || !strings.Contains(err.Error(), "label") {
		t.Fatalf("err = %v, want the label shape-drift error naming the endpoint", err)
	}
}

// The teardown DELETE renders the removed row flat (a bare object —
// no JSON:API envelope) and decodes harmlessly; a gone row is the
// typed 404 — the caller's already-gone signal.
func TestRemoveInstanceHostname(t *testing.T) {
	var gotPath, gotMethod string
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		fmt.Fprint(w, `{"id": "host-1"}`)
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	if err := c.RemoveInstanceHostname(context.Background(), "host-1"); err != nil {
		t.Fatalf("RemoveInstanceHostname: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v1/instance-hostnames/host-1" {
		t.Errorf("request = %s %s, want DELETE /api/v1/instance-hostnames/host-1", gotMethod, gotPath)
	}

	gone := testServer(t, http.StatusNotFound, `{"errors": [{"status": "404"}]}`)
	c2 := New(gone.URL, "fvpat_test", "test", gone.Client())
	err := c2.RemoveInstanceHostname(context.Background(), "host-1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("err = %v, want a 404 *APIError (the already-gone signal)", err)
	}
}
