package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// PocketIdWiring is the durable record that an instance's sign-in is
// wired to a PocketID (#731 T3): which PocketID served it, which OIDC
// client and client secret the platform minted there, and when. The
// ids are identifiers, never credentials — the secret's VALUE lives
// once in the instance's env_vars and is never echoed.
//
// The three mutation routes render this struct FLAT (the generic
// action's body — no JSON:API envelope); the read renders a JSON:API
// document whose data.id is the wiring ROW id. The row id is
// deliberately not carried here: it changes on every re-wire, so it
// must never become a Terraform identity (the instance id is the
// one-wiring key).
type PocketIdWiring struct {
	InstanceID         string
	PocketIDInstanceID string
	PocketIDClientID   string
	PocketIDSecretID   string
	WiredAt            string
}

// DisconnectResult is the flat POST /disconnect-pocketid body:
// client_deleted is false on the idempotent no-wiring arm (and when a
// best-effort PocketID delete is refused) — either way the platform
// side is disconnected.
type DisconnectResult struct {
	InstanceID    string
	ClientDeleted bool
}

// pocketIdWiringFromFlat decodes the generic-action body the three
// mutation routes render. The platform's action output is a plain
// object at the top level (never a data/attributes envelope), so a
// map + the typed attribute readers is the honest shape check.
func pocketIdWiringFromFlat(route string, body []byte) (PocketIdWiring, error) {
	var attrs map[string]any
	if err := json.Unmarshal(body, &attrs); err != nil {
		return PocketIdWiring{}, fmt.Errorf("decoding response from %s: %w", route, err)
	}
	instanceID, err := attrString(attrs, "instance_id")
	if err != nil {
		return PocketIdWiring{}, fmt.Errorf("%s returned a drifted body: %w", route, err)
	}
	pocketIDInstanceID, err := attrString(attrs, "pocket_id_instance_id")
	if err != nil {
		return PocketIdWiring{}, fmt.Errorf("%s returned a drifted body: %w", route, err)
	}
	clientID, err := attrString(attrs, "pocket_id_client_id")
	if err != nil {
		return PocketIdWiring{}, fmt.Errorf("%s returned a drifted body: %w", route, err)
	}
	secretID, err := attrString(attrs, "pocket_id_secret_id")
	if err != nil {
		return PocketIdWiring{}, fmt.Errorf("%s returned a drifted body: %w", route, err)
	}
	wiredAt, err := attrString(attrs, "wired_at")
	if err != nil {
		return PocketIdWiring{}, fmt.Errorf("%s returned a drifted body: %w", route, err)
	}
	return PocketIdWiring{
		InstanceID:         instanceID,
		PocketIDInstanceID: pocketIDInstanceID,
		PocketIDClientID:   clientID,
		PocketIDSecretID:   secretID,
		WiredAt:            wiredAt,
	}, nil
}

// pocketIdWiringFromResource decodes the read's JSON:API document. The
// attribute set is exactly the five pinned fields; data.id (the wiring
// row id) is intentionally ignored — see PocketIdWiring.
func pocketIdWiringFromResource(row Resource) (PocketIdWiring, error) {
	instanceID, err := attrString(row.Attributes, "instance_id")
	if err != nil {
		return PocketIdWiring{}, err
	}
	pocketIDInstanceID, err := attrString(row.Attributes, "pocket_id_instance_id")
	if err != nil {
		return PocketIdWiring{}, err
	}
	clientID, err := attrString(row.Attributes, "pocket_id_client_id")
	if err != nil {
		return PocketIdWiring{}, err
	}
	secretID, err := attrString(row.Attributes, "pocket_id_secret_id")
	if err != nil {
		return PocketIdWiring{}, err
	}
	wiredAt, err := attrString(row.Attributes, "wired_at")
	if err != nil {
		return PocketIdWiring{}, err
	}
	return PocketIdWiring{
		InstanceID:         instanceID,
		PocketIDInstanceID: pocketIDInstanceID,
		PocketIDClientID:   clientID,
		PocketIDSecretID:   secretID,
		WiredAt:            wiredAt,
	}, nil
}

// ConnectPocketId calls POST /api/v1/instances/:id/connect-pocketid —
// the dispatch: a not-yet-deployed instance ships the SSO env on its
// next deploy, a deployed one rides the retrofit (row + live
// workload). The argument rides at the data level, the generic action
// argument shape open_api.json pins (not inside data.attributes). A
// 400 is the platform's own refusal — busy rollouts, the feature
// flag off, custody — surfaced verbatim by the caller.
func (c *Client) ConnectPocketId(ctx context.Context, instanceID, pocketIDInstanceID string) (PocketIdWiring, error) {
	path := "/api/v1/instances/" + instanceID + "/connect-pocketid"
	body, err := c.send(ctx, http.MethodPost, path, map[string]any{
		"data": map[string]any{"pocket_id_instance_id": pocketIDInstanceID},
	})
	if err != nil {
		return PocketIdWiring{}, err
	}
	wiring, err := pocketIdWiringFromFlat("POST "+path, body)
	if err != nil {
		return PocketIdWiring{}, err
	}
	return wiring, nil
}

// GetPocketIdWiring calls GET /api/v1/instances/:instance_id/pocket-id-wiring.
// The read stays available while the SSO feature flag is off (a
// provider refresh must not lose state during a shutdown window); an
// unwired instance is the 404 the resource's Read turns into
// removed-from-state.
func (c *Client) GetPocketIdWiring(ctx context.Context, instanceID string) (PocketIdWiring, error) {
	path := "/api/v1/instances/" + instanceID + "/pocket-id-wiring"
	doc, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return PocketIdWiring{}, err
	}
	wiring, err := pocketIdWiringFromResource(doc.Data)
	if err != nil {
		return PocketIdWiring{}, shapeDrift("GET "+path, doc.Data.ID, err)
	}
	return wiring, nil
}

// DisconnectPocketId calls POST /api/v1/instances/:id/disconnect-pocketid
// — the best-effort PocketID client delete, the env strip (row + live
// workload), then the wiring clear. IDEMPOTENT by contract: no wiring
// is a success with client_deleted false, never a 404 — a Terraform
// destroy of an already-gone wiring converges.
func (c *Client) DisconnectPocketId(ctx context.Context, instanceID string) (DisconnectResult, error) {
	path := "/api/v1/instances/" + instanceID + "/disconnect-pocketid"
	body, err := c.send(ctx, http.MethodPost, path, nil)
	if err != nil {
		return DisconnectResult{}, err
	}
	var attrs map[string]any
	if err := json.Unmarshal(body, &attrs); err != nil {
		return DisconnectResult{}, fmt.Errorf("decoding response from POST %s: %w", path, err)
	}
	gotInstanceID, err := attrString(attrs, "instance_id")
	if err != nil {
		return DisconnectResult{}, fmt.Errorf("POST %s returned a drifted body: %w", path, err)
	}
	deleted, err := attrOptBool(attrs, "client_deleted")
	if err != nil {
		return DisconnectResult{}, fmt.Errorf("POST %s returned a drifted body: %w", path, err)
	}
	if deleted == nil {
		return DisconnectResult{}, fmt.Errorf("POST %s returned a drifted body: client_deleted is missing or null", path)
	}
	return DisconnectResult{InstanceID: gotInstanceID, ClientDeleted: *deleted}, nil
}

// RotatePocketIdSecret calls POST /api/v1/instances/:id/rotate-pocketid-secret
// — the zero-downtime rotation: a second active secret, the running
// workload carries it, then the old secret dies. The flat body is the
// wiring with the new pocket_id_secret_id. A not-yet-deployed (or
// lost/deleting) wiring is the platform's 400, surfaced verbatim.
func (c *Client) RotatePocketIdSecret(ctx context.Context, instanceID string) (PocketIdWiring, error) {
	path := "/api/v1/instances/" + instanceID + "/rotate-pocketid-secret"
	body, err := c.send(ctx, http.MethodPost, path, nil)
	if err != nil {
		return PocketIdWiring{}, err
	}
	wiring, err := pocketIdWiringFromFlat("POST "+path, body)
	if err != nil {
		return PocketIdWiring{}, err
	}
	return wiring, nil
}
