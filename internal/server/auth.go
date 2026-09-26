// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

const tokenCookie = "hops_token"

// authorized says whether a request may pass when a token is configured: a bearer header (senders), or the
// cookie the page sets from `/#token=...` (browsers; EventSource can send a cookie but not a header).
// The health check is always open, so a monitor can see the process without the secret.
func (s *Server) authorized(r *http.Request) bool {
	if s.Token == "" || r.URL.Path == "/api/v1/health" {
		return true
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return equal(strings.TrimPrefix(h, "Bearer "), s.Token)
	}
	if c, err := r.Cookie(tokenCookie); err == nil {
		return equal(c.Value, s.Token)
	}
	return false
}

func equal(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// setCookie answers `POST /api/v1/session` with the token in the body's Authorization header: the page calls
// it once with the token from its address, and from then on the cookie carries it.
func (s *Server) setCookie(w http.ResponseWriter, r *http.Request) {
	if s.Token == "" {
		writeJSON(w, map[string]any{"ok": true, "token": false})
		return
	}
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") || !equal(strings.TrimPrefix(h, "Bearer "), s.Token) {
		http.Error(w, "wrong token", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: tokenCookie, Value: s.Token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 30 * 86400})
	writeJSON(w, map[string]any{"ok": true, "token": true})
}
