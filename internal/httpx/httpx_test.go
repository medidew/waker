package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAuth pins the bearer-token gate: correct token passes through,
// anything else 401s without touching next, and an empty token disables the
// check entirely. A bare token (no "Bearer " prefix) still matches — the
// check is a TrimPrefix compare, which is the existing behaviour.
func TestAuth(t *testing.T) {
	for _, tc := range []struct {
		name       string
		token      string
		header     string
		wantCode   int
		wantServed bool
	}{
		{"correct bearer", "sekret", "Bearer sekret", 200, true},
		{"bare token also matches", "sekret", "sekret", 200, true},
		{"wrong token", "sekret", "Bearer nope", 401, false},
		{"missing header", "sekret", "", 401, false},
		{"open (empty token) passes through", "", "", 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			served := false
			h := Auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				served = true
			}), tc.token)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rr.Code, tc.wantCode)
			}
			if served != tc.wantServed {
				t.Fatalf("next served = %v, want %v", served, tc.wantServed)
			}
		})
	}
}

// TestAuthErrorBody pins the JSON error shape that agent and server 401s
// carry.
func TestAuthErrorBody(t *testing.T) {
	h := Auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), "tok")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body["error"] != "missing or invalid token" {
		t.Fatalf("error = %q, want %q", body["error"], "missing or invalid token")
	}
}

// TestWriteJSON pins the content type, implicit 200, and body encoding.
func TestWriteJSON(t *testing.T) {
	rr := httptest.NewRecorder()
	WriteJSON(rr, map[string]int{"n": 1})

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var got map[string]int
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["n"] != 1 {
		t.Fatalf("body = %+v, want n=1", got)
	}
}

// TestWriteErr pins the status code, content type, and {"error": msg} shape.
func TestWriteErr(t *testing.T) {
	rr := httptest.NewRecorder()
	WriteErr(rr, http.StatusServiceUnavailable, "boom")

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "boom" {
		t.Fatalf("error = %q, want %q", body["error"], "boom")
	}
}
