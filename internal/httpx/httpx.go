// Package httpx holds small HTTP helpers shared by the waker binaries.
package httpx

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// Auth wraps next with a token check when token is non-empty. The token may
// be presented as an "Authorization: Bearer" header or, when cookieName is
// non-empty, as an HttpOnly cookie of that name (the dashboard presents its
// waker_auth cookie this way, since JavaScript cannot read it). Comparisons
// are constant-time.
func Auth(next http.Handler, token string, cookieName string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" &&
			!tokenMatches(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), token) &&
			!cookieMatches(r, cookieName, token) {
			WriteErr(w, http.StatusUnauthorized, "missing or invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func tokenMatches(got, token string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func cookieMatches(r *http.Request, name, token string) bool {
	if name == "" {
		return false
	}
	c, err := r.Cookie(name)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(token)) == 1
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
