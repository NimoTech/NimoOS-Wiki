// Package rootsync is the HTTP client NimoOS-Wiki uses to push root
// authorization to the NimoOS core.
//
// Authz-source decoupling background: core is the sole authorization
// authority (the o_root_grants table); create/delete/enable/disable
// operations on root directories on the Wiki side need to be pushed to core,
// which decides which directories the agent can access based on that. The
// push is best-effort: any failed call is marked needsReconcile on the
// corresponding root by the caller (Task 5's manager), and covered by a full
// Reconcile at Wiki service startup — so this package does no retry/backoff,
// it just faithfully converts the result of a single HTTP call (2xx / non-2xx
// / network error) into nil/error.
package rootsync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// requestTimeout is the timeout for a single push request. Core is a
// LAN-local service, so 3s is plenty; a timeout is also treated as a
// failure, leaving the caller to set needsReconcile.
const requestTimeout = 3 * time.Second

// Grant is a single root authorization record pushed to core; its JSON tags
// must exactly match the request-body fields of core's
// /v1/nimoos/_internal/root-grants endpoints.
type Grant struct {
	RootID  string `json:"root_id"`
	Path    string `json:"path"`
	Enabled bool   `json:"enabled"`
}

// Client is the core authz push client.
type Client struct {
	discoveryFile string
	httpClient    *http.Client
}

// New builds a Client. discoveryFile is the service-discovery file path
// (written by core at startup, recording core's current listen address);
// production always passes /var/run/nimoos/nimoos.url, while tests can pass
// any temp file path to inject a fake core address.
func New(baseURLFile string) *Client {
	return &Client{
		discoveryFile: baseURLFile,
		httpClient:    &http.Client{Timeout: requestTimeout},
	}
}

// Upsert incrementally pushes one root grant (create or update), mapping to
// the core endpoint PUT {base}/v1/nimoos/_internal/root-grants/{root_id}.
func (c *Client) Upsert(ctx context.Context, g Grant) error {
	body := struct {
		Path    string `json:"path"`
		Enabled bool   `json:"enabled"`
	}{Path: g.Path, Enabled: g.Enabled}
	url := fmt.Sprintf("%s/v1/nimoos/_internal/root-grants/%s", resolveBaseURL(c.discoveryFile), g.RootID)
	return c.do(ctx, http.MethodPut, url, body)
}

// Delete removes one root grant, mapping to the core endpoint
// DELETE {base}/v1/nimoos/_internal/root-grants/{root_id}.
func (c *Client) Delete(ctx context.Context, rootID string) error {
	url := fmt.Sprintf("%s/v1/nimoos/_internal/root-grants/%s", resolveBaseURL(c.discoveryFile), rootID)
	return c.do(ctx, http.MethodDelete, url, nil)
}

// Reconcile pushes a full reconcile pass (typically called once at Wiki
// service startup), mapping to the core endpoint
// POST {base}/v1/nimoos/_internal/root-grants/reconcile with body
// {"grants":[...]}. Core treats this list as authoritative and syncs all
// rows with source="wiki".
func (c *Client) Reconcile(ctx context.Context, grants []Grant) error {
	body := struct {
		Grants []Grant `json:"grants"`
	}{Grants: grants}
	url := fmt.Sprintf("%s/v1/nimoos/_internal/root-grants/reconcile", resolveBaseURL(c.discoveryFile))
	return c.do(ctx, http.MethodPost, url, body)
}

// do is the request-sending logic shared by all three methods: short
// timeout, JSON-encoded request body, and any non-2xx status code is
// converted to an error (response body details aren't parsed — the caller
// only cares about success/failure).
func (c *Client) do(ctx context.Context, method, url string, payload any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	var reader *bytes.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("rootsync: failed to marshal request body: %w", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return fmt.Errorf("rootsync: failed to build request: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("rootsync: request to core failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("rootsync: core returned non-success status code %d", resp.StatusCode)
	}
	return nil
}
