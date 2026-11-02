package auth

import (
	"net/http"
	"time"

	"github.com/workbuddy/backend/internal/platform/httpx"
)

// Auth cookie names. The access cookie is sent on every request; the refresh
// cookie only to /auth/*, so it reaches POST /auth/refresh and /auth/logout
// and nothing else.
const (
	AccessTokenCookie  = "access_token"
	RefreshTokenCookie = "refresh_token"

	accessCookiePath  = "/"
	refreshCookiePath = "/auth"
)

// CookieConfig controls the auth cookies. The zero value gives host-only,
// non-Secure cookies, which is what local development over http needs.
type CookieConfig struct {
	// Secure sets the Secure attribute (COOKIE_SECURE, default true in
	// production).
	Secure bool
	// Domain sets the Domain attribute (COOKIE_DOMAIN), e.g. ".example.com";
	// empty means host-only.
	Domain string
}

func (c CookieConfig) cookie(name, value, path string, maxAge time.Duration) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		Domain:   c.Domain,
		MaxAge:   int(maxAge / time.Second),
		HttpOnly: true,
		Secure:   c.Secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// setAuthCookies stores a fresh token pair in the access and refresh
// cookies, each living as long as its token.
func (c CookieConfig) setAuthCookies(w http.ResponseWriter, pair *TokenPair, refreshTTL time.Duration) {
	http.SetCookie(w, c.cookie(AccessTokenCookie, pair.AccessToken, accessCookiePath, time.Duration(pair.ExpiresIn)*time.Second))
	http.SetCookie(w, c.cookie(RefreshTokenCookie, pair.RefreshToken, refreshCookiePath, refreshTTL))
}

// clearAuthCookies expires both auth cookies; path and domain must match the
// ones they were set with for the browser to drop them.
func (c CookieConfig) clearAuthCookies(w http.ResponseWriter) {
	for _, ck := range []*http.Cookie{
		c.cookie(AccessTokenCookie, "", accessCookiePath, 0),
		c.cookie(RefreshTokenCookie, "", refreshCookiePath, 0),
	} {
		ck.MaxAge = -1
		http.SetCookie(w, ck)
	}
}

// isUnsafeMethod reports whether a request can change state, so needs the
// Origin check when it is authenticated by a cookie.
func isUnsafeMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// originAllowed is the CSRF check for cookie-authenticated requests: a
// request without an Origin header (same-origin GETs, non-browser clients)
// passes, a browser request must come from an allowed origin.
func originAllowed(r *http.Request, allowed *httpx.Origins) bool {
	origin := r.Header.Get("Origin")
	return origin == "" || allowed.Allowed(origin)
}
