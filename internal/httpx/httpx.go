// Package httpx holds small HTTP helpers shared by the waker binaries.
package httpx

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Auth wraps next with a bearer-token check when token is non-empty.
func Auth(next http.Handler, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got != token {
				WriteErr(w, http.StatusUnauthorized, "missing or invalid token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// WriteJSON writes v as a JSON response with 200 OK.
func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// WriteErr writes a JSON error response.
func WriteErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
