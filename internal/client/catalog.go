package client

import (
	"context"
	"fmt"
	"net/http"
)

// App is one row of GET /api/v1/apps (and GET /api/v1/apps/:slug) —
// the catalog's public browse surface (app.ex's default_fields).
type App struct {
	ID          string
	Name        string
	Slug        string
	Tagline     *string
	Description *string
	IconURL     *string
	SourceURL   *string
	DocsURL     *string
	FileAccess  bool
}

func appFromResource(row Resource) (App, error) {
	name, err := attrString(row.Attributes, "name")
	if err != nil {
		return App{}, err
	}
	slug, err := attrString(row.Attributes, "slug")
	if err != nil {
		return App{}, err
	}
	fileAccess, err := attrOptBool(row.Attributes, "file_access")
	if err != nil {
		return App{}, err
	}
	if fileAccess == nil {
		return App{}, fmt.Errorf("file_access is missing or null")
	}
	tagline, err := attrOptString(row.Attributes, "tagline")
	if err != nil {
		return App{}, err
	}
	description, err := attrOptString(row.Attributes, "description")
	if err != nil {
		return App{}, err
	}
	iconURL, err := attrOptString(row.Attributes, "icon_url")
	if err != nil {
		return App{}, err
	}
	sourceURL, err := attrOptString(row.Attributes, "source_url")
	if err != nil {
		return App{}, err
	}
	docsURL, err := attrOptString(row.Attributes, "docs_url")
	if err != nil {
		return App{}, err
	}
	return App{
		ID:          row.ID,
		Name:        name,
		Slug:        slug,
		Tagline:     tagline,
		Description: description,
		IconURL:     iconURL,
		SourceURL:   sourceURL,
		DocsURL:     docsURL,
		FileAccess:  *fileAccess,
	}, nil
}

// ListApps calls GET /api/v1/apps — the catalog, sorted by name.
func (c *Client) ListApps(ctx context.Context) ([]App, error) {
	doc, err := c.DoList(ctx, http.MethodGet, "/api/v1/apps", nil)
	if err != nil {
		return nil, err
	}
	apps := make([]App, 0, len(doc.Data))
	for _, row := range doc.Data {
		app, err := appFromResource(row)
		if err != nil {
			return nil, shapeDrift("GET /api/v1/apps", row.ID, err)
		}
		apps = append(apps, app)
	}
	return apps, nil
}

// GetAppBySlug calls GET /api/v1/apps/:slug — the slug-keyed lookup.
// A 404 is the *APIError the caller surfaces (an unknown slug is the
// caller's typo, the server's wording is the right diagnostic).
func (c *Client) GetAppBySlug(ctx context.Context, slug string) (App, error) {
	doc, err := c.Do(ctx, http.MethodGet, "/api/v1/apps/"+slug, nil)
	if err != nil {
		return App{}, err
	}
	app, err := appFromResource(doc.Data)
	if err != nil {
		return App{}, shapeDrift("GET /api/v1/apps/"+slug, doc.Data.ID, err)
	}
	return app, nil
}

// AppVersion is one row of GET /api/v1/apps/:id/versions — the deploy
// blueprint summary (app_version.ex's default_fields). The mixed-type
// spec objects (configurable_env_vars, first_run) stay unmapped — no
// flat Terraform type fits them; the panel is their surface.
type AppVersion struct {
	ID               string
	AppID            string
	Version          string
	Image            string
	Port             int64
	HealthcheckPath  string
	StorageMountPath string
	DefaultCPUCores  *float64
	DefaultMemoryMB  *int64
	DefaultStorageGB *int64
	DefaultEnvVars   map[string]string
	ReleaseNotes     *string
	PublishedAt      *string
}

func appVersionFromResource(row Resource) (AppVersion, error) {
	appID, err := attrString(row.Attributes, "app_id")
	if err != nil {
		return AppVersion{}, err
	}
	version, err := attrString(row.Attributes, "version")
	if err != nil {
		return AppVersion{}, err
	}
	image, err := attrString(row.Attributes, "image")
	if err != nil {
		return AppVersion{}, err
	}
	port, err := attrOptInt64(row.Attributes, "port")
	if err != nil {
		return AppVersion{}, err
	}
	if port == nil {
		return AppVersion{}, fmt.Errorf("port is missing or null")
	}
	healthcheck, err := attrOptString(row.Attributes, "healthcheck_path")
	if err != nil {
		return AppVersion{}, err
	}
	if healthcheck == nil {
		return AppVersion{}, fmt.Errorf("healthcheck_path is missing or null")
	}
	mountPath, err := attrOptString(row.Attributes, "storage_mount_path")
	if err != nil {
		return AppVersion{}, err
	}
	if mountPath == nil {
		return AppVersion{}, fmt.Errorf("storage_mount_path is missing or null")
	}
	cpu, err := attrOptFloat64(row.Attributes, "default_cpu_cores")
	if err != nil {
		return AppVersion{}, err
	}
	mem, err := attrOptInt64(row.Attributes, "default_memory_mb")
	if err != nil {
		return AppVersion{}, err
	}
	storage, err := attrOptInt64(row.Attributes, "default_storage_gb")
	if err != nil {
		return AppVersion{}, err
	}
	envVars, err := attrStringMap(row.Attributes, "default_env_vars")
	if err != nil {
		return AppVersion{}, err
	}
	notes, err := attrOptString(row.Attributes, "release_notes")
	if err != nil {
		return AppVersion{}, err
	}
	published, err := attrOptString(row.Attributes, "published_at")
	if err != nil {
		return AppVersion{}, err
	}
	return AppVersion{
		ID:               row.ID,
		AppID:            appID,
		Version:          version,
		Image:            image,
		Port:             *port,
		HealthcheckPath:  *healthcheck,
		StorageMountPath: *mountPath,
		DefaultCPUCores:  cpu,
		DefaultMemoryMB:  mem,
		DefaultStorageGB: storage,
		DefaultEnvVars:   envVars,
		ReleaseNotes:     notes,
		PublishedAt:      published,
	}, nil
}

// ListAppVersions calls GET /api/v1/apps/:app_id/versions — newest
// version first (the action sorts version desc).
func (c *Client) ListAppVersions(ctx context.Context, appID string) ([]AppVersion, error) {
	doc, err := c.DoList(ctx, http.MethodGet, "/api/v1/apps/"+appID+"/versions", nil)
	if err != nil {
		return nil, err
	}
	versions := make([]AppVersion, 0, len(doc.Data))
	for _, row := range doc.Data {
		v, err := appVersionFromResource(row)
		if err != nil {
			return nil, shapeDrift("GET /api/v1/apps/"+appID+"/versions", row.ID, err)
		}
		versions = append(versions, v)
	}
	return versions, nil
}

// FindAppVersion returns the version whose version string matches.
func FindAppVersion(versions []AppVersion, version string) (AppVersion, bool) {
	for _, v := range versions {
		if v.Version == version {
			return v, true
		}
	}
	return AppVersion{}, false
}
