package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// backupDocument is the routed row shape (backup_setting.ex
// default_fields) — the write-only secret never appears here except to
// prove the parser ignores a key it must never carry.
func backupDocument(id string) string {
	return `{"data": {"id": "` + id + `", "type": "backup_setting", "attributes": {
		"instance_id": "inst-1", "endpoint": "https://account.r2.cloudflarestorage.com",
		"bucket": "fluxvale-kavita", "prefix": "fluxvale/app",
		"access_key_id": "AKIAIOSFODNN7EXAMPLE", "retention_keep": 7, "enabled": true,
		"secret_access_key": "HOSTILE"
	}}}`
}

// The create save: everything rides data.attributes; instance_id rides
// the PATH only (the route refuses a body copy outright), the secret
// rides among the coordinates, unset optionals are omitted.
func TestSaveBackupSettingBodyShape(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, backupDocument("bs-1"))
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	retention := int64(14)
	enabled := false
	row, err := c.SaveBackupSetting(context.Background(), "inst-1", BackupSettingInput{
		Endpoint:        "https://account.r2.cloudflarestorage.com",
		Bucket:          "fluxvale-kavita",
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		RetentionKeep:   &retention,
		Enabled:         &enabled,
		SecretAccessKey: "provider-secret-value",
	})
	if err != nil {
		t.Fatalf("SaveBackupSetting: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/api/v1/instances/inst-1/backup-setting" {
		t.Errorf("request = %s %s, want POST by instance path", gotMethod, gotPath)
	}
	data, _ := gotBody["data"].(map[string]any)
	if data["type"] != "backup_setting" {
		t.Fatalf("data = %+v, want type backup_setting", gotBody["data"])
	}
	if _, ok := data["instance_id"]; ok {
		t.Error("instance_id at the data level — the path is the only intake")
	}
	attrs := data["attributes"].(map[string]any)
	if attrs["secret_access_key"] != "provider-secret-value" {
		t.Errorf("secret_access_key = %v, want the write-only argument inside attributes", attrs["secret_access_key"])
	}
	if _, ok := attrs["instance_id"]; ok {
		t.Error("instance_id in attributes — the route refuses a body copy (invalid_body)")
	}
	if attrs["retention_keep"] != 14.0 || attrs["enabled"] != false {
		t.Errorf("retention/enabled = %v/%v, want 14/false", attrs["retention_keep"], attrs["enabled"])
	}
	if _, ok := attrs["prefix"]; ok {
		t.Error("prefix sent with a nil pointer — omit at create")
	}

	if row.ID != "bs-1" || row.InstanceID != "inst-1" || row.RetentionKeep != 7 {
		t.Errorf("row = %+v, want the parsed row (retention from the doc, not the input)", row)
	}
	if row.Prefix == nil || *row.Prefix != "fluxvale/app" {
		t.Errorf("Prefix = %v, want the parsed prefix", row.Prefix)
	}
}

// The update save: the row id rides the document, the full field set
// rides the attributes, a cleared prefix is an explicit null, and a
// blank secret is omitted (keep the stored credential).
func TestUpdateBackupSettingBodyShape(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		fmt.Fprint(w, backupDocument("bs-1"))
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	retention := int64(14)
	enabled := true
	if _, err := c.UpdateBackupSetting(context.Background(), "bs-1", BackupSettingInput{
		Endpoint:      "https://account.r2.cloudflarestorage.com",
		Bucket:        "fluxvale-kavita",
		AccessKeyID:   "AKIAIOSFODNN7EXAMPLE",
		RetentionKeep: &retention,
		Enabled:       &enabled,
	}); err != nil {
		t.Fatalf("UpdateBackupSetting: %v", err)
	}

	if gotMethod != http.MethodPatch || gotPath != "/api/v1/backup-settings/bs-1" {
		t.Errorf("request = %s %s, want PATCH by row path", gotMethod, gotPath)
	}
	data := gotBody["data"].(map[string]any)
	if data["id"] != "bs-1" {
		t.Errorf("data.id = %v, want the row id", data["id"])
	}
	attrs := data["attributes"].(map[string]any)
	for _, field := range []string{"endpoint", "bucket", "prefix", "access_key_id", "retention_keep", "enabled"} {
		if _, ok := attrs[field]; !ok {
			t.Errorf("attributes.%s missing — the update is a full save", field)
		}
	}
	if v, ok := attrs["prefix"]; !ok || v != nil {
		t.Errorf("prefix = %v (present %t), want an explicit null", v, ok)
	}
	if _, ok := attrs["secret_access_key"]; ok {
		t.Error("secret_access_key sent with a blank input — blank keeps the stored credential")
	}
}

func TestUpdateBackupSettingRotatesSecret(t *testing.T) {
	var gotBody map[string]any
	srv := handlerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		fmt.Fprint(w, backupDocument("bs-1"))
	}))

	c := New(srv.URL, "fvpat_test", "test", srv.Client())
	if _, err := c.UpdateBackupSetting(context.Background(), "bs-1", BackupSettingInput{
		Endpoint: "e", Bucket: "b", AccessKeyID: "k",
		SecretAccessKey: "rotated",
	}); err != nil {
		t.Fatalf("UpdateBackupSetting: %v", err)
	}

	attrs := gotBody["data"].(map[string]any)["attributes"].(map[string]any)
	if attrs["secret_access_key"] != "rotated" {
		t.Errorf("secret_access_key = %v, want the rotation value", attrs["secret_access_key"])
	}
}

func TestGetBackupSetting404IsTyped(t *testing.T) {
	srv := testServer(t, http.StatusNotFound, `{"errors": [{"status": "404"}]}`)
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	_, err := c.GetBackupSetting(context.Background(), "inst-1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("err = %v, want a 404 *APIError (the removed-from-state signal)", err)
	}
}

func TestBackupSettingShapeDriftIsAnError(t *testing.T) {
	srv := testServer(t, http.StatusOK, `{"data": {"id": "bs-1", "type": "backup_setting",
		"attributes": {"instance_id": "inst-1", "endpoint": "e", "bucket": "b",
		"access_key_id": "k", "retention_keep": "seven", "enabled": true}}}`)
	c := New(srv.URL, "fvpat_test", "test", srv.Client())

	if _, err := c.GetBackupSetting(context.Background(), "inst-1"); err == nil {
		t.Fatal("err = nil, want the retention_keep shape-drift error")
	}
}
