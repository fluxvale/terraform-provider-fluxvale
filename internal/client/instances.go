package client

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
)

// Instance is the detail surface GET /api/v1/instances/:id renders —
// and the POST create and the lifecycle PATCH verbs return the same
// list (@detail_fields, instance.ex). env_vars and status_message are
// hide_fields'd off EVERY response; the struct has no fields for them,
// so no code path can echo them.
//
// GetInstance additionally requests the wire's public FK attributes
// (organization_id, app_version_id) via a sparse fieldset — public,
// but not in @detail_fields; ash_json_api's fields[] parameter
// replaces the route's default list (request.ex: Map.get(
// request.fields, resource) || route.default_fields).
type Instance struct {
	ID             string
	Name           string
	Slug           string
	Namespace      *string
	Status         string
	Exposure       string
	AlwaysOn       bool
	Image          *string
	Port           *int64
	CPUCores       *float64
	MemoryMB       *int64
	StorageGB      *int64
	DeployedAt     *string
	OrganizationID string
	AppVersionID   string
}

// detailFields is the sparse fieldset GetInstance requests: the route's
// @detail_fields plus the two public FK ids import converges on. The
// create/PATCH responses keep the route default and omit them; only
// GetInstance asks.
const detailFields = "name,slug,namespace,status,exposure,always_on,image,port," +
	"healthcheck_path,storage_mount_path,file_access,cpu_cores,memory_mb,storage_gb," +
	"deployed_at,organization_id,app_version_id"

func instanceFromResource(row Resource) (Instance, error) {
	name, err := attrString(row.Attributes, "name")
	if err != nil {
		return Instance{}, err
	}
	slug, err := attrString(row.Attributes, "slug")
	if err != nil {
		return Instance{}, err
	}
	status, err := attrString(row.Attributes, "status")
	if err != nil {
		return Instance{}, err
	}
	exposure, err := attrString(row.Attributes, "exposure")
	if err != nil {
		return Instance{}, err
	}
	alwaysOn, err := attrOptBool(row.Attributes, "always_on")
	if err != nil {
		return Instance{}, err
	}
	if alwaysOn == nil {
		return Instance{}, fmt.Errorf("always_on is missing or null")
	}
	// The deploy-derived fields are null on a pending row — absent, not
	// drift. The FK ids are absent from the create/PATCH responses'
	// route-default field set; only GetInstance asks for them.
	namespace, err := attrOptString(row.Attributes, "namespace")
	if err != nil {
		return Instance{}, err
	}
	image, err := attrOptString(row.Attributes, "image")
	if err != nil {
		return Instance{}, err
	}
	port, err := attrOptInt64(row.Attributes, "port")
	if err != nil {
		return Instance{}, err
	}
	cpu, err := attrOptFloat64(row.Attributes, "cpu_cores")
	if err != nil {
		return Instance{}, err
	}
	memory, err := attrOptInt64(row.Attributes, "memory_mb")
	if err != nil {
		return Instance{}, err
	}
	storage, err := attrOptInt64(row.Attributes, "storage_gb")
	if err != nil {
		return Instance{}, err
	}
	deployedAt, err := attrOptString(row.Attributes, "deployed_at")
	if err != nil {
		return Instance{}, err
	}
	orgID, _ := attrOptString(row.Attributes, "organization_id")
	versionID, _ := attrOptString(row.Attributes, "app_version_id")
	return Instance{
		ID:             row.ID,
		Name:           name,
		Slug:           slug,
		Namespace:      namespace,
		Status:         status,
		Exposure:       exposure,
		AlwaysOn:       *alwaysOn,
		Image:          image,
		Port:           port,
		CPUCores:       cpu,
		MemoryMB:       memory,
		StorageGB:      storage,
		DeployedAt:     deployedAt,
		OrganizationID: deref(orgID),
		AppVersionID:   deref(versionID),
	}, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// CreateInstanceInput is the POST /api/v1/instances body. Every field
// rides inside data.attributes — app_version_id, organization_id, and
// env_vars included (the latter is the write-only :env_vars argument;
// ash_json_api routes the same-named input there — instance_route_
// test.exs, #503).
type CreateInstanceInput struct {
	Name           string
	CPUCores       float64
	MemoryMB       int64
	StorageGB      int64
	AppVersionID   string
	OrganizationID string            // "" omits — the server falls back to the actor's personal org
	EnvVars        map[string]string // nil omits — the server merges catalog defaults
}

// CreateInstance calls POST /api/v1/instances. The response is the
// detail surface — status is "pending"; deploying is a separate PATCH
// (the deploy trigger enqueues from :deploy, not :create).
func (c *Client) CreateInstance(ctx context.Context, in CreateInstanceInput) (Instance, error) {
	// cpu_cores is a :decimal on the platform — the wire type is a
	// string (open_api.json), so 0.5 ships as "0.5".
	attrs := map[string]any{
		"name":           in.Name,
		"cpu_cores":      strconv.FormatFloat(in.CPUCores, 'f', -1, 64),
		"memory_mb":      in.MemoryMB,
		"storage_gb":     in.StorageGB,
		"app_version_id": in.AppVersionID,
	}
	if in.OrganizationID != "" {
		attrs["organization_id"] = in.OrganizationID
	}
	if in.EnvVars != nil {
		attrs["env_vars"] = in.EnvVars
	}

	doc, err := c.Do(ctx, http.MethodPost, "/api/v1/instances", map[string]any{
		"data": map[string]any{"type": "instance", "attributes": attrs},
	})
	if err != nil {
		return Instance{}, err
	}
	instance, err := instanceFromResource(doc.Data)
	if err != nil {
		return Instance{}, shapeDrift("POST /api/v1/instances", doc.Data.ID, err)
	}
	return instance, nil
}

// GetInstance calls GET /api/v1/instances/:id with a sparse fieldset
// — @detail_fields plus the public FK ids (organization_id,
// app_version_id), which ash_json_api's fields[] swaps in place of the
// route's default list. A 404 is the *APIError (Status 404) —
// removed-from-state for the resource's Read.
func (c *Client) GetInstance(ctx context.Context, id string) (Instance, error) {
	path := "/api/v1/instances/" + id + "?fields[instance]=" + detailFields
	doc, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return Instance{}, err
	}
	instance, err := instanceFromResource(doc.Data)
	if err != nil {
		return Instance{}, shapeDrift("GET /api/v1/instances/"+id, doc.Data.ID, err)
	}
	return instance, nil
}

// InstanceAction PATCHes a lifecycle verb — deploy, stop, start,
// always-on, exposure — the route set instance.ex pins. The body
// carries the resource id plus the attributes object, exactly the
// shape the route tests send (empty attributes for the no-input
// verbs). The response is the detail surface; stop/start are
// synchronous scale flips (the response already carries the new
// status), deploy only enqueues.
func (c *Client) InstanceAction(ctx context.Context, id, action string, attrs map[string]any) (Instance, error) {
	doc, err := c.Do(ctx, http.MethodPatch, "/api/v1/instances/"+id+"/"+action, map[string]any{
		"data": map[string]any{"type": "instance", "id": id, "attributes": attrs},
	})
	if err != nil {
		return Instance{}, err
	}
	instance, err := instanceFromResource(doc.Data)
	if err != nil {
		return Instance{}, shapeDrift("PATCH /api/v1/instances/"+id+"/"+action, doc.Data.ID, err)
	}
	return instance, nil
}

// ResizeInstance PATCHes the stopped-first resize (#493, ADR-0043):
// CPU/RAM both ways, storage up-only (a shrink is the server's 400
// naming both figures). All three dims ride explicitly — absent ones
// keep the row's value, so this is exactly the plan's allocation. The
// caller owns the stopped-first sequencing (the guard refuses anything
// but :stopped).
func (c *Client) ResizeInstance(ctx context.Context, id string, cpuCores float64, memoryMB, storageGB int64) (Instance, error) {
	return c.InstanceAction(ctx, id, "resize", map[string]any{
		"cpu_cores":  strconv.FormatFloat(cpuCores, 'f', -1, 64),
		"memory_mb":  memoryMB,
		"storage_gb": storageGB,
	})
}

// DeleteInstance calls DELETE /api/v1/instances/:id — the generic
// :delete (async teardown; the response renders flat, no JSON:API
// envelope, which decodes as an empty Document harmlessly).
func (c *Client) DeleteInstance(ctx context.Context, id string) error {
	_, err := c.Do(ctx, http.MethodDelete, "/api/v1/instances/"+id, nil)
	return err
}
