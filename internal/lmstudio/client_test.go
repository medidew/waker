package lmstudio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient builds a Client whose base points at a fake upstream. The
// test lives in-package so it can reach the unexported base field; New()'s
// http clients are kept as production wires them.
func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	c := New(0)
	c.base = ts.URL
	return c
}

// TestNewDefaultPort pins the documented default when no port is given.
func TestNewDefaultPort(t *testing.T) {
	if got := New(0).base; got != "http://127.0.0.1:1234" {
		t.Fatalf("New(0).base = %q, want %q", got, "http://127.0.0.1:1234")
	}
}

// TestModelsWrappedShape covers the {"data": [...]} response envelope.
func TestModelsWrappedShape(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/models" {
			t.Errorf("path = %q, want /api/v0/models", r.URL.Path)
		}
		fmt.Fprint(w, `{"data":[{"id":"m1","type":"Llama","arch":"llama","quantization":"Q4","state":"loaded","max_context_length":8192}]}`)
	}))

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("len(models) = %d, want 1", len(models))
	}
	m := models[0]
	if m.ID != "m1" || m.Type != "Llama" || m.Arch != "llama" || m.Quantization != "Q4" || m.State != "loaded" || m.MaxContext != 8192 {
		t.Fatalf("model = %+v, want full m1", m)
	}
}

// TestModelsDirectArray covers the bare-array response shape and the
// fallback to /v1/models when /api/v0/models does not answer with 200.
func TestModelsDirectArray(t *testing.T) {
	var v0Hits, v1Hits int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/models":
			v0Hits++
			w.WriteHeader(http.StatusNotFound)
		case "/v1/models":
			v1Hits++
			fmt.Fprint(w, `[{"id":"m2"}]`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if v0Hits != 1 || v1Hits != 1 {
		t.Fatalf("hits = {v0:%d v1:%d}, want {1 1}", v0Hits, v1Hits)
	}
	if len(models) != 1 || models[0].ID != "m2" {
		t.Fatalf("models = %+v, want m2", models)
	}
}

// TestModelsUnreachable pins the error the agent maps to a 502: a client
// pointed at a port nothing listens on must fail fast with the
// "unreachable" error rather than hang. Port 1 gets an instant RST on
// loopback.
func TestModelsUnreachable(t *testing.T) {
	c := New(1)
	_, err := c.Models(context.Background())
	if err == nil {
		t.Fatal("Models against a dead port: want error, got nil")
	}
	if !strings.Contains(err.Error(), "LM Studio server unreachable") {
		t.Fatalf("error = %v, want the unreachable error", err)
	}
}

// TestModelsUnrecognizedShape: a 200 answer in a shape neither decoder
// understands is reported per-path, and Models surfaces the last attempt's
// error.
func TestModelsUnrecognizedShape(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"models":[]}`)
	}))
	_, err := c.Models(context.Background())
	if err == nil {
		t.Fatal("Models with unrecognized shape: want error, got nil")
	}
	for _, frag := range []string{"LM Studio server unreachable", "GET /v1/models: unrecognized response shape"} {
		if !strings.Contains(err.Error(), frag) {
			t.Fatalf("error = %q, want it to contain %q", err, frag)
		}
	}
}

// TestLoad pins the POST shape of Load: path, JSON content type, payload.
func TestLoad(t *testing.T) {
	var gotPath, gotCT, gotModel string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		var p map[string]string
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode load payload: %v", err)
		}
		gotModel = p["model"]
		w.WriteHeader(http.StatusAccepted)
	}))

	if err := c.Load(context.Background(), "m1"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if gotPath != "/api/v1/models/load" {
		t.Fatalf("path = %q, want /api/v1/models/load", gotPath)
	}
	if gotCT != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", gotCT)
	}
	if gotModel != "m1" {
		t.Fatalf("model = %q, want m1", gotModel)
	}
}

// TestLoadErrors pins Load's two failure faces: an upstream 5xx is a
// status error, an unreachable server is the "unreachable" error the agent
// turns into a 502.
func TestLoadErrors(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	if err := c.Load(context.Background(), "m1"); err == nil || !strings.Contains(err.Error(), "POST /api/v1/models/load: status 500") {
		t.Fatalf("Load with 500 upstream: error = %v, want the status-500 error", err)
	}

	dead := New(1)
	if err := dead.Load(context.Background(), "m1"); err == nil || !strings.Contains(err.Error(), "LM Studio server unreachable") {
		t.Fatalf("Load against a dead port: error = %v, want the unreachable error", err)
	}
}

// TestUnload pins Unload's lookup-then-unload flow: it lists models, finds
// the loaded one, and POSTs its id as instance_id.
func TestUnload(t *testing.T) {
	var gotPath, gotInstance string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/models":
			fmt.Fprint(w, `{"data":[{"id":"a","state":"not-loaded"},{"id":"b","state":"loaded"}]}`)
		case "/api/v1/models/unload":
			gotPath = r.URL.Path
			var p map[string]string
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				t.Errorf("decode unload payload: %v", err)
			}
			gotInstance = p["instance_id"]
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))

	if err := c.Unload(context.Background()); err != nil {
		t.Fatalf("Unload: %v", err)
	}
	if gotPath != "/api/v1/models/unload" {
		t.Fatalf("path = %q, want /api/v1/models/unload", gotPath)
	}
	if gotInstance != "b" {
		t.Fatalf("instance_id = %q, want b", gotInstance)
	}
}

// TestUnloadNothingLoaded: when no model reports state "loaded", Unload must
// say so rather than fabricate an instance to unload.
func TestUnloadNothingLoaded(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"a","state":"not-loaded"}]}`)
	}))
	err := c.Unload(context.Background())
	if err == nil || err.Error() != "no model is currently loaded" {
		t.Fatalf("Unload with nothing loaded: error = %v, want the no-model error", err)
	}
}
