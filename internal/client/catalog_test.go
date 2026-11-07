package client

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestListOrganizationsShape(t *testing.T) {
	doc := `{"data": [
		{"id": "org-1", "type": "organization", "attributes": {"slug": "acme", "name": "Acme"}},
		{"id": "org-2", "type": "organization", "attributes": {"slug": "b-co", "name": "B Co"}}
	]}`
	srv := testServer(t, http.StatusOK, doc)
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	orgs, err := c.ListOrganizations(context.Background())
	if err != nil {
		t.Fatalf("ListOrganizations: %v", err)
	}
	if len(orgs) != 2 {
		t.Fatalf("orgs = %+v, want 2 rows", orgs)
	}
	// id at the resource level; slug + name in attributes — the route
	// test's key set.
	if orgs[0].ID != "org-1" || orgs[0].Slug != "acme" || orgs[0].Name != "Acme" {
		t.Errorf("orgs[0] = %+v, want org-1/acme/Acme", orgs[0])
	}
}

func TestListOrganizationsShapeDrift(t *testing.T) {
	doc := `{"data": [{"id": "org-1", "type": "organization", "attributes": {"name": "Acme"}}]}`
	srv := testServer(t, http.StatusOK, doc)
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	if _, err := c.ListOrganizations(context.Background()); err == nil || !strings.Contains(err.Error(), "slug") {
		t.Fatalf("err = %v, want the slug shape-drift error", err)
	}
}

func TestListAppsMapsTheBrowseSurface(t *testing.T) {
	doc := `{"data": [
		{"id": "app-1", "type": "app", "attributes": {"name": "Forgejo", "slug": "forgejo", "tagline": "Git forge",
			"description": "Self-hosted", "icon_url": "https://x/i.png", "source_url": "https://src", "docs_url": "https://docs", "file_access": false}},
		{"id": "app-2", "type": "app", "attributes": {"name": "Memos", "slug": "memos", "file_access": true}}
	]}`
	srv := testServer(t, http.StatusOK, doc)
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	apps, err := c.ListApps(context.Background())
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 2 {
		t.Fatalf("apps = %+v, want 2 rows", apps)
	}
	first := apps[0]
	if first.ID != "app-1" || first.Slug != "forgejo" || first.Tagline == nil || *first.Tagline != "Git forge" {
		t.Errorf("apps[0] = %+v, want the mapped app-1 row", first)
	}
	// Sparse row: absent optionals parse as absent.
	second := apps[1]
	if second.Tagline != nil || second.Description != nil || second.IconURL != nil {
		t.Errorf("apps[1] optionals = %+v, want nil", second)
	}
	if !second.FileAccess {
		t.Errorf("apps[1].FileAccess = false, want true")
	}
}

func TestGetAppBySlugPathAnd404(t *testing.T) {
	var gotPath string
	srv := handlerServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"data": {"id": "app-1", "type": "app", "attributes": {"name": "Forgejo", "slug": "forgejo", "file_access": false}}}`))
	})
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	app, err := c.GetAppBySlug(context.Background(), "forgejo")
	if err != nil {
		t.Fatalf("GetAppBySlug: %v", err)
	}
	if gotPath != "/api/v1/apps/forgejo" {
		t.Errorf("path = %q, want /api/v1/apps/forgejo", gotPath)
	}
	if app.ID != "app-1" || app.Slug != "forgejo" {
		t.Errorf("app = %+v, want app-1/forgejo", app)
	}

	notFound := testServer(t, http.StatusNotFound, `{"errors": [{"title": "Not Found", "detail": "unknown app"}]}`)
	c2 := New(notFound.URL, "fvpat_test", "test", notFound.Client())
	if _, err := c2.GetAppBySlug(context.Background(), "nope"); err == nil {
		t.Fatal("unknown slug: err = nil, want the server's 404")
	}
}

func TestListAppVersionsPathAndDecimals(t *testing.T) {
	var gotPath string
	doc := `{"data": [
		{"id": "ver-2", "type": "app_version", "attributes": {"app_id": "app-1", "version": "2.0", "image": "reg/app:2.0",
			"port": 3000, "healthcheck_path": "/", "storage_mount_path": "/data",
			"default_cpu_cores": "1.5", "default_memory_mb": 512, "default_storage_gb": 2,
			"default_env_vars": {"FOO": "bar"}, "release_notes": "new", "published_at": "2026-02-01T00:00:00Z"}},
		{"id": "ver-1", "type": "app_version", "attributes": {"app_id": "app-1", "version": "1.0", "image": "reg/app:1.0",
			"port": 3000, "healthcheck_path": "/", "storage_mount_path": "/data",
			"default_cpu_cores": "0.5", "default_memory_mb": 256, "default_storage_gb": 0,
			"default_env_vars": {}, "release_notes": null, "published_at": null}}
	]}`
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(doc))
	}))
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	versions, err := c.ListAppVersions(context.Background(), "app-1")
	if err != nil {
		t.Fatalf("ListAppVersions: %v", err)
	}
	if gotPath != "/api/v1/apps/app-1/versions" {
		t.Errorf("path = %q, want /api/v1/apps/app-1/versions", gotPath)
	}
	if len(versions) != 2 {
		t.Fatalf("versions = %+v, want 2 rows", versions)
	}
	// Decimal-as-string parses; the mixed-type spec objects stay
	// unmapped without erroring.
	if versions[0].DefaultCPUCores == nil || *versions[0].DefaultCPUCores != 1.5 {
		t.Errorf("DefaultCPUCores = %v, want 1.5 from \"1.5\"", versions[0].DefaultCPUCores)
	}
	if versions[0].DefaultEnvVars["FOO"] != "bar" {
		t.Errorf("DefaultEnvVars = %v, want FOO=bar", versions[0].DefaultEnvVars)
	}
	if versions[1].ReleaseNotes != nil || versions[1].PublishedAt != nil {
		t.Errorf("versions[1] nullables = %v/%v, want nil", versions[1].ReleaseNotes, versions[1].PublishedAt)
	}
	if versions[1].DefaultStorageGB == nil || *versions[1].DefaultStorageGB != 0 {
		t.Errorf("DefaultStorageGB = %v, want 0", versions[1].DefaultStorageGB)
	}

	if _, ok := FindAppVersion(versions, "1.0"); !ok {
		t.Error("FindAppVersion(1.0): not found")
	}
	if _, ok := FindAppVersion(versions, "9.9"); ok {
		t.Error("FindAppVersion(9.9): found, want miss")
	}
}
