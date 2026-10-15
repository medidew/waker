// Package lmstudio talks to a local LM Studio instance over its HTTP API,
// falling back to the lms CLI where useful.
package lmstudio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// Client targets a local LM Studio server and its lms CLI.
type Client struct {
	base   string
	httpc  *http.Client
	lmsBin string
}

// New creates a client for the LM Studio server on localhost:port.
func New(port int, lmsBin string) *Client {
	if port <= 0 {
		port = 1234
	}
	if lmsBin == "" {
		lmsBin = "lms"
	}
	return &Client{
		base:   fmt.Sprintf("http://127.0.0.1:%d", port),
		httpc:  &http.Client{Timeout: 8 * time.Second},
		lmsBin: lmsBin,
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

// Models lists available models. Prefers the local API (which also reports
// which model is loaded), falling back to parsing `lms ls`.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	var lastErr error
	for _, path := range []string{"/api/v0/models", "/v1/models"} {
		models, err := c.modelsFromAPI(ctx, path)
		if err == nil {
			return models, nil
		}
		lastErr = err
	}
	models, cliErr := c.modelsViaCLI(ctx)
	if cliErr != nil {
		return nil, fmt.Errorf("local API: %v; lms ls: %w", lastErr, cliErr)
	}
	return models, nil
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

func (c *Client) modelsViaCLI(ctx context.Context) ([]Model, error) {
	out, err := runOutput(ctx, c.lmsBin, "ls")
	if err != nil {
		return nil, err
	}
	var models []Model
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "IDENTIFIER") || strings.HasPrefix(line, "---") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		id := f[0]
		if !strings.Contains(id, "/") && !strings.ContainsAny(id, ".-") {
			continue // header or junk row
		}
		models = append(models, Model{ID: id})
	}
	return models, nil
}

// Load loads a model into the running server. Tries the API first, then CLI.
func (c *Client) Load(ctx context.Context, model string) error {
	payload, _ := json.Marshal(map[string]string{"model": model})
	var apiErr error
	resp, doErr := c.postJSON(ctx, "/api/v0/load", payload)
	if doErr != nil {
		apiErr = doErr
	} else if resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		apiErr = fmt.Errorf("status %d", resp.StatusCode)
	}
	if apiErr == nil {
		return nil
	}
	out, cliErr := runOutput(ctx, c.lmsBin, "load", model)
	if cliErr != nil {
		return fmt.Errorf("api: %v; lms load: %s (%v)", apiErr, tail(out), cliErr)
	}
	return nil
}

// Unload removes the current model from memory. Tries API endpoints first,
// then the CLI.
func (c *Client) Unload(ctx context.Context) error {
	var apiErr error
	for _, method := range []string{"DELETE", "POST"} {
		req, err := http.NewRequestWithContext(ctx, method, c.base+"/api/v0/unload", nil)
		if err != nil {
			apiErr = err
			continue
		}
		resp, doErr := c.httpc.Do(req)
		if doErr != nil {
			apiErr = doErr
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode < 300 {
			return nil
		}
		apiErr = fmt.Errorf("%s /api/v0/unload: status %d", method, resp.StatusCode)
	}
	out, cliErr := runOutput(ctx, c.lmsBin, "unload")
	if cliErr != nil {
		return fmt.Errorf("api: %v; lms unload: %s (%v)", apiErr, tail(out), cliErr)
	}
	return nil
}

func (c *Client) postJSON(ctx context.Context, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.httpc.Do(req)
}

func runOutput(ctx context.Context, argv ...string) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), err
	}
	return buf.String(), nil
}

func tail(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " | "))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "(no output)"
	}
	return s
}
