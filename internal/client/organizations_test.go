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

func organizationDocument(slug, name string) string {
	return `{"data": {"id": "org-1", "type": "organization", "attributes": {
		"slug": "` + slug + `", "name": "` + name + `"
	}}}`
}

func TestCreateOrganizationBodyShape(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, organizationDocument("acme", "Acme"))
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	org, err := c.CreateOrganization(context.Background(), CreateOrganizationInput{Name: "Acme", Slug: "acme"})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/api/v1/organizations" {
		t.Errorf("request = %s %s, want POST /api/v1/organizations", gotMethod, gotPath)
	}
	data := gotBody["data"].(map[string]any)
	if data["type"] != "organization" {
		t.Errorf("type = %v, want organization", data["type"])
	}
	attrs := data["attributes"].(map[string]any)
	if attrs["name"] != "Acme" || attrs["slug"] != "acme" {
		t.Errorf("attrs = %v, want name/slug only", attrs)
	}

	if org.ID != "org-1" || org.Slug != "acme" || org.Name != "Acme" {
		t.Errorf("org = %+v, want org-1 acme Acme", org)
	}
}

func TestGetOrganization(t *testing.T) {
	var gotPath string
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprint(w, organizationDocument("acme", "Acme"))
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	org, err := c.GetOrganization(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("GetOrganization: %v", err)
	}
	if gotPath != "/api/v1/organizations/org-1" {
		t.Errorf("path = %q, want /api/v1/organizations/org-1", gotPath)
	}
	if org.Name != "Acme" {
		t.Errorf("name = %q, want Acme", org.Name)
	}
}

func TestGetOrganizationBySlug(t *testing.T) {
	var gotPath string
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprint(w, organizationDocument("acme", "Acme"))
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	org, err := c.GetOrganizationBySlug(context.Background(), "acme")
	if err != nil {
		t.Fatalf("GetOrganizationBySlug: %v", err)
	}
	if gotPath != "/api/v1/organizations/by-slug/acme" {
		t.Errorf("path = %q, want the by-slug route", gotPath)
	}
	if org.ID != "org-1" {
		t.Errorf("id = %q, want org-1", org.ID)
	}
}

func TestUpdateOrganizationBodyShape(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		fmt.Fprint(w, organizationDocument("acme", "Renamed"))
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	org, err := c.UpdateOrganization(context.Background(), "org-1", "Renamed")
	if err != nil {
		t.Fatalf("UpdateOrganization: %v", err)
	}

	if gotMethod != http.MethodPatch || gotPath != "/api/v1/organizations/org-1" {
		t.Errorf("request = %s %s, want PATCH /api/v1/organizations/org-1", gotMethod, gotPath)
	}
	data := gotBody["data"].(map[string]any)
	if data["id"] != "org-1" {
		t.Errorf("body id = %v, want org-1", data["id"])
	}
	attrs := data["attributes"].(map[string]any)
	if attrs["name"] != "Renamed" {
		t.Errorf("name = %v, want Renamed", attrs["name"])
	}
	if _, present := attrs["slug"]; present {
		t.Error("PATCH carried slug — the provider's update is name-only")
	}
	if org.Name != "Renamed" {
		t.Errorf("org name = %q, want Renamed", org.Name)
	}
}

func TestDeleteOrganizationSurfacesGuard(t *testing.T) {
	var gotPath, gotMethod string
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"errors": [{"status": "400", "detail": "This organization still has 1 instance. Delete it first."}]}`)
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	err := c.DeleteOrganization(context.Background(), "org-1")
	if err == nil {
		t.Fatal("DeleteOrganization succeeded, want the guard refusal")
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v1/organizations/org-1" {
		t.Errorf("request = %s %s, want DELETE /api/v1/organizations/org-1", gotMethod, gotPath)
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("err = %v, want a 400 APIError", err)
	}
	if !strings.Contains(err.Error(), "still has 1 instance") {
		t.Errorf("err = %q, want the server's guard message verbatim", err)
	}
}

func TestGetOrganizationBySlugEscapesTheImportKey(t *testing.T) {
	var gotPath string
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		fmt.Fprint(w, organizationDocument("acme", "Acme"))
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	if _, err := c.GetOrganizationBySlug(context.Background(), "acme/other"); err != nil {
		t.Fatalf("GetOrganizationBySlug: %v", err)
	}
	if gotPath != "/api/v1/organizations/by-slug/acme%2Fother" {
		t.Errorf("path = %q, want the slug escaped as one segment", gotPath)
	}
}

func TestOrganizationFromResourceShapeDrift(t *testing.T) {
	cases := []struct {
		name string
		row  Resource
		want string
	}{
		{
			name: "no id",
			row:  Resource{Attributes: map[string]any{"slug": "acme", "name": "Acme"}},
			want: "no id",
		},
		{
			name: "missing slug",
			row:  Resource{ID: "org-1", Attributes: map[string]any{"name": "Acme"}},
			want: "slug is missing or null",
		},
		{
			name: "missing name",
			row:  Resource{ID: "org-1", Attributes: map[string]any{"slug": "acme"}},
			want: "name is missing or null",
		},
		{
			name: "name type",
			row:  Resource{ID: "org-1", Attributes: map[string]any{"slug": "acme", "name": 7}},
			want: "name is int, want string",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := organizationFromResource(tc.row, "GET /test")
			if err == nil {
				t.Fatal("organizationFromResource accepted a drifted row")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want something containing %q", err, tc.want)
			}
		})
	}
}
