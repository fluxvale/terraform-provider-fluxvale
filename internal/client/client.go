// Package client is the provider's minimal typed JSON:API client over
// the FluxVale /api/v1 surface (ADR-0019 Am. 4): bearer PAT auth,
// JSON:API documents in, typed errors out. No retries in v0 — the API
// rate-limits at 300 req/min per token (ADR-0019 Am. 1) and a plain
// 429 surfaces as an error diagnostic.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	contentTypeJSONAPI = "application/vnd.api+json"
	userAgentFormat    = "terraform-provider-fluxvale/%s (+https://github.com/fluxvale/fluxvale)"
)

// Client calls the FluxVale API with a bearer PAT.
type Client struct {
	baseURL   string
	token     string
	userAgent string
	http      *http.Client
}

// New builds a client. httpClient may be nil (http.DefaultClient);
// tests pass the httptest server's client.
func New(baseURL, token, version string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		baseURL:   strings.TrimSuffix(baseURL, "/"),
		token:     token,
		userAgent: fmt.Sprintf(userAgentFormat, version),
		http:      httpClient,
	}
}

// ErrorObject is one member of a JSON:API errors array.
type ErrorObject struct {
	Title  string `json:"title,omitempty"`
	Detail string `json:"detail,omitempty"`
	Code   string `json:"code,omitempty"`
}

// APIError is a failed FluxVale API call: the HTTP status plus the
// server's JSON:API errors array when it sent one. Bodies that don't
// decode (an HTML page from a proxy) keep a trimmed snippet.
type APIError struct {
	Status  int
	Errors  []ErrorObject
	rawSnip string
}

func (e *APIError) Error() string {
	if len(e.Errors) == 0 {
		if e.rawSnip != "" {
			return fmt.Sprintf("fluxvale API: status %d: %s", e.Status, e.rawSnip)
		}
		return fmt.Sprintf("fluxvale API: status %d", e.Status)
	}
	msgs := make([]string, 0, len(e.Errors))
	for _, eo := range e.Errors {
		switch {
		case eo.Title != "" && eo.Detail != "":
			msgs = append(msgs, eo.Title+": "+eo.Detail)
		case eo.Title != "":
			msgs = append(msgs, eo.Title)
		default:
			msgs = append(msgs, eo.Detail)
		}
	}
	return fmt.Sprintf("fluxvale API: status %d: %s", e.Status, strings.Join(msgs, "; "))
}

// Document is a JSON:API document whose data is one resource.
// Includes, links, and relationships stay untyped until a slice needs
// them.
type Document struct {
	Data   Resource      `json:"data"`
	Errors []ErrorObject `json:"errors,omitempty"`
}

// ListDocument is a JSON:API document whose data is a collection.
type ListDocument struct {
	Data   []Resource    `json:"data"`
	Errors []ErrorObject `json:"errors,omitempty"`
}

// Resource is a JSON:API resource object. Attributes stay loose — the
// provider maps only what each slice needs.
type Resource struct {
	ID         string         `json:"id,omitempty"`
	Type       string         `json:"type,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// Do performs an API call and decodes a single-resource JSON:API
// document. A nil body is a plain request (GET). Non-2xx responses
// decode the JSON:API errors array into an *APIError.
func (c *Client) Do(ctx context.Context, method, path string, body any) (Document, error) {
	resBody, err := c.send(ctx, method, path, body)
	if err != nil {
		return Document{}, err
	}
	var doc Document
	if err := json.Unmarshal(resBody, &doc); err != nil {
		return Document{}, fmt.Errorf("decoding response from %s %s: %w", method, path, err)
	}
	return doc, nil
}

// DoList is Do for collection documents ({"data": […]}).
func (c *Client) DoList(ctx context.Context, method, path string, body any) (ListDocument, error) {
	resBody, err := c.send(ctx, method, path, body)
	if err != nil {
		return ListDocument{}, err
	}
	var doc ListDocument
	if err := json.Unmarshal(resBody, &doc); err != nil {
		return ListDocument{}, fmt.Errorf("decoding response from %s %s: %w", method, path, err)
	}
	return doc, nil
}

// send performs the call and returns the raw response body — the one
// transport path Do and DoList decode over. Non-2xx bodies become an
// *APIError.
func (c *Client) send(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding request body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", contentTypeJSONAPI)
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", contentTypeJSONAPI)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s %s: %w", method, path, err)
	}
	defer res.Body.Close()

	resBody, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response from %s %s: %w", method, path, err)
	}

	if res.StatusCode >= http.StatusBadRequest {
		return nil, decodeAPIError(res.StatusCode, resBody)
	}
	return resBody, nil
}

// Me is the authenticated user from GET /api/v1/me — the provider's
// auth ping. The route is scopeless (ADR-0019 Am. 1): any live token
// works, so it proves the credential without needing any grants.
type Me struct {
	ID    string
	Email string
}

// Me calls GET /api/v1/me.
func (c *Client) Me(ctx context.Context) (Me, error) {
	doc, err := c.Do(ctx, http.MethodGet, "/api/v1/me", nil)
	if err != nil {
		return Me{}, err
	}

	email, _ := doc.Data.Attributes["email"].(string)
	if doc.Data.ID == "" {
		return Me{}, fmt.Errorf("GET /api/v1/me returned no id — API shape drift")
	}
	if email == "" {
		return Me{}, fmt.Errorf("GET /api/v1/me returned no email — API shape drift")
	}
	return Me{ID: doc.Data.ID, Email: email}, nil
}

// decodeAPIError turns a non-2xx body into an *APIError. Undecodable
// bodies still produce a typed error carrying the HTTP status.
func decodeAPIError(status int, body []byte) *APIError {
	errOut := &APIError{Status: status}
	var doc Document
	if json.Unmarshal(body, &doc) == nil && len(doc.Errors) > 0 {
		errOut.Errors = doc.Errors
		return errOut
	}
	const snip = 200
	if raw := strings.TrimSpace(string(body)); raw != "" {
		if len(raw) > snip {
			raw = raw[:snip]
		}
		errOut.rawSnip = raw
	}
	return errOut
}
