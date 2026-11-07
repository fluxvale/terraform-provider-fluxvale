package client

import (
	"context"
	"fmt"
	"net/http"
)

// BackupSetting is one instance's backup destination row — GET
// /api/v1/instances/:instance_id/backup-setting and the routed saves
// render the same fields (backup_setting.ex default_fields). The
// write-only secret_access_key argument has no field here: it never
// rides any response.
type BackupSetting struct {
	ID            string
	InstanceID    string
	Endpoint      string
	Bucket        string
	Prefix        *string
	AccessKeyID   string
	RetentionKeep int64
	Enabled       bool
}

func backupSettingFromResource(row Resource) (BackupSetting, error) {
	instanceID, err := attrString(row.Attributes, "instance_id")
	if err != nil {
		return BackupSetting{}, err
	}
	endpoint, err := attrString(row.Attributes, "endpoint")
	if err != nil {
		return BackupSetting{}, err
	}
	bucket, err := attrString(row.Attributes, "bucket")
	if err != nil {
		return BackupSetting{}, err
	}
	accessKeyID, err := attrString(row.Attributes, "access_key_id")
	if err != nil {
		return BackupSetting{}, err
	}
	retention, err := attrOptInt64(row.Attributes, "retention_keep")
	if err != nil {
		return BackupSetting{}, err
	}
	if retention == nil {
		return BackupSetting{}, fmt.Errorf("retention_keep is missing or null")
	}
	enabled, err := attrOptBool(row.Attributes, "enabled")
	if err != nil {
		return BackupSetting{}, err
	}
	if enabled == nil {
		return BackupSetting{}, fmt.Errorf("enabled is missing or null")
	}
	prefix, err := attrOptString(row.Attributes, "prefix")
	if err != nil {
		return BackupSetting{}, err
	}
	return BackupSetting{
		ID:            row.ID,
		InstanceID:    instanceID,
		Endpoint:      endpoint,
		Bucket:        bucket,
		Prefix:        prefix,
		AccessKeyID:   accessKeyID,
		RetentionKeep: *retention,
		Enabled:       *enabled,
	}, nil
}

// BackupSettingInput is the routed save body (the POST create and the
// PATCH update share the field set, plus the write-only secret). nil
// optionals omit at create — the server defaults apply (retention 7,
// enabled true); at update every field rides so a cleared prefix lands
// as an explicit null.
type BackupSettingInput struct {
	Endpoint        string
	Bucket          string
	Prefix          *string
	AccessKeyID     string
	RetentionKeep   *int64
	Enabled         *bool
	SecretAccessKey string // "" omits — absent/blank keeps the stored credential
}

func (in BackupSettingInput) createAttrs() map[string]any {
	attrs := map[string]any{
		"endpoint":      in.Endpoint,
		"bucket":        in.Bucket,
		"access_key_id": in.AccessKeyID,
	}
	if in.Prefix != nil {
		attrs["prefix"] = *in.Prefix
	}
	if in.RetentionKeep != nil {
		attrs["retention_keep"] = *in.RetentionKeep
	}
	if in.Enabled != nil {
		attrs["enabled"] = *in.Enabled
	}
	if in.SecretAccessKey != "" {
		attrs["secret_access_key"] = in.SecretAccessKey
	}
	return attrs
}

func (in BackupSettingInput) updateAttrs() map[string]any {
	attrs := map[string]any{
		"endpoint":      in.Endpoint,
		"bucket":        in.Bucket,
		"access_key_id": in.AccessKeyID,
	}
	if in.RetentionKeep != nil {
		attrs["retention_keep"] = *in.RetentionKeep
	}
	if in.Enabled != nil {
		attrs["enabled"] = *in.Enabled
	}
	// An explicit null clears the prefix; a value rewrites it.
	if in.Prefix != nil {
		attrs["prefix"] = *in.Prefix
	} else {
		attrs["prefix"] = nil
	}
	if in.SecretAccessKey != "" {
		attrs["secret_access_key"] = in.SecretAccessKey
	}
	return attrs
}

// SaveBackupSetting calls POST /api/v1/instances/:instance_id/backup-setting
// — the panel-equivalent save. instance_id rides the PATH only: the
// route refuses a body copy outright (backup_route_test's
// invalid_body). The save converges the cluster server-side (secret
// keep-or-rotate, repo URL recompute, quota margin) — no client-side
// cluster work, no polling: a cluster failure is the 5xx.
func (c *Client) SaveBackupSetting(ctx context.Context, instanceID string, in BackupSettingInput) (BackupSetting, error) {
	doc, err := c.Do(ctx, http.MethodPost, "/api/v1/instances/"+instanceID+"/backup-setting", map[string]any{
		"data": map[string]any{"type": "backup_setting", "attributes": in.createAttrs()},
	})
	if err != nil {
		return BackupSetting{}, err
	}
	row, err := backupSettingFromResource(doc.Data)
	if err != nil {
		return BackupSetting{}, shapeDrift("POST /api/v1/instances/"+instanceID+"/backup-setting", doc.Data.ID, err)
	}
	return row, nil
}

// UpdateBackupSetting PATCHes /api/v1/backup-settings/:id — all fields
// plus the secret when set (a new value rotates; absent/blank keeps
// the stored credential).
func (c *Client) UpdateBackupSetting(ctx context.Context, rowID string, in BackupSettingInput) (BackupSetting, error) {
	doc, err := c.Do(ctx, http.MethodPatch, "/api/v1/backup-settings/"+rowID, map[string]any{
		"data": map[string]any{"type": "backup_setting", "id": rowID, "attributes": in.updateAttrs()},
	})
	if err != nil {
		return BackupSetting{}, err
	}
	row, err := backupSettingFromResource(doc.Data)
	if err != nil {
		return BackupSetting{}, shapeDrift("PATCH /api/v1/backup-settings/"+rowID, doc.Data.ID, err)
	}
	return row, nil
}

// GetBackupSetting calls GET /api/v1/instances/:instance_id/backup-setting
// — the read is by INSTANCE, not by the row's id. A 404 is the
// *APIError (Status 404): the instance has no settings row —
// removed-from-state for the resource's Read.
func (c *Client) GetBackupSetting(ctx context.Context, instanceID string) (BackupSetting, error) {
	doc, err := c.Do(ctx, http.MethodGet, "/api/v1/instances/"+instanceID+"/backup-setting", nil)
	if err != nil {
		return BackupSetting{}, err
	}
	row, err := backupSettingFromResource(doc.Data)
	if err != nil {
		return BackupSetting{}, shapeDrift("GET /api/v1/instances/"+instanceID+"/backup-setting", doc.Data.ID, err)
	}
	return row, nil
}

// DeleteBackupSetting calls DELETE /api/v1/backup-settings/:id — the
// "stop backing up entirely" row delete; a plain row delete with no
// cluster state (the namespace Secret persists by design).
func (c *Client) DeleteBackupSetting(ctx context.Context, rowID string) error {
	_, err := c.Do(ctx, http.MethodDelete, "/api/v1/backup-settings/"+rowID, nil)
	return err
}
