// Package lmstudio talks to a local LM Studio instance over its HTTP API.
// All operations require the server to be running; there is deliberately no
// lms CLI fallback, because CLI commands can start the daemon as a side
// effect — a passive model-list poll must never resurrect services.
package lmstudio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client targets the local LM Studio server.
type Client struct {
	base  string
	httpc *http.Client
}

// New creates a client for the LM Studio server on localhost:port.
func New(port int) *Client {
	if port <= 0 {
		port = 1234
	}
	return &Client{
		base:  fmt.Sprintf("http://127.0.0.1:%d", port),
		httpc: &http.Client{Timeout: 8 * time.Second},
	}
}

// Model is one entry from LM Studio's model list.
type Model struct {
	ID           string `json:"id"`
	Type         string `json:"type,omitempty"`
	Arch         string `json:"arch,omitempty"`
	Quantization string `json:"quantization,omitempty"`
	State        string `json:"state,omitempty"` // "loaded" | "not-loaded"
	MaxContext   int    `json:"max_context_length,omitempty"`
}

// Models lists available models via the local API. Returns an error if the
// server is not reachable instead of falling back to the CLI.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	var lastErr error
	for _, path := range []string{"/api/v0/models", "/v1/models"} {
		models, err := c.modelsFromAPI(ctx, path)
		if err == nil {
			return models, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("LM Studio server unreachable: %v (start the LM Studio Server service)", lastErr)
}

func (c *Client) modelsFromAPI(ctx context.Context, path string) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	var wrap struct {
		Data []Model `json:"data"`
	}
	if err := json.Unmarshal(body, &wrap); err == nil && len(wrap.Data) > 0 {
		return wrap.Data, nil
	}
	var direct []Model
	if err := json.Unmarshal(body, &direct); err == nil && len(direct) > 0 {
		return direct, nil
	}
	return nil, fmt.Errorf("GET %s: unrecognized response shape", path)
}

// Load loads a model into the running server.
func (c *Client) Load(ctx context.Context, model string) error {
	payload, _ := json.Marshal(map[string]string{"model": model})
	resp, err := c.postJSON(ctx, "/api/v0/load", payload)
	if err != nil {
		return fmt.Errorf("LM Studio server unreachable: %v (start the LM Studio Server service)", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("POST /api/v0/load: status %d", resp.StatusCode)
	}
	return nil
}

// Unload removes the current model from memory. Tries DELETE then POST on
// /api/v0/unload (endpoint method varies by LM Studio version).
func (c *Client) Unload(ctx context.Context) error {
	var lastErr error
	for _, method := range []string{"DELETE", "POST"} {
		req, err := http.NewRequestWithContext(ctx, method, c.base+"/api/v0/unload", nil)
		if err != nil {
			lastErr = err
			continue
		}
		resp, doErr := c.httpc.Do(req)
		if doErr != nil {
			lastErr = doErr
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("%s /api/v0/unload: status %d", method, resp.StatusCode)
	}
	return fmt.Errorf("LM Studio server unreachable: %v (start the LM Studio Server service)", lastErr)
}

func (c *Client) postJSON(ctx context.Context, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.httpc.Do(req)
}
