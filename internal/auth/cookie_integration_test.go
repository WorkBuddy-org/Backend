package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/workbuddy/backend/internal/auth"
	"github.com/workbuddy/backend/internal/config"
	"github.com/workbuddy/backend/internal/httpapi"
	"github.com/workbuddy/backend/internal/platform/httpx"
	"github.com/workbuddy/backend/internal/platform/postgres"
	redisplatform "github.com/workbuddy/backend/internal/platform/redis"
	"golang.org/x/oauth2"
)

const (
	testFrontendOrigin = "http://localhost:3000"
	testCallbackURL    = "http://localhost:3000/auth/callback"
)

type cookieEnv struct {
	t      *testing.T
	cfg    *config.Config
	router http.Handler
}

func newCookieEnv(t *testing.T, cookies auth.CookieConfig, oauthCfg auth.OAuthConfig) *cookieEnv {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}

	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	db, err := postgres.NewPool(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	t.Cleanup(db.Close)
	redisClient, err := redisplatform.NewClient(cfg.RedisURL)
	if err != nil {
		t.Fatalf("connect redis: %v", err)
	}
	t.Cleanup(func() { _ = redisClient.Close() })

	tokens := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTAccessTTL)
	service := auth.NewService(auth.NewRepository(db), tokens, auth.NewOAuthRegistry(oauthCfg), cfg.JWTRefreshTTL, testCallbackURL, redisClient)
	origins := httpx.NewOrigins([]string{testFrontendOrigin})
	handler := auth.NewHandler(service)
	handler.SetCookieConfig(cookies)
	handler.SetAllowedOrigins(origins)
	mw := auth.NewMiddleware(tokens)
	mw.SetAllowedOrigins(origins)

	router := httpapi.NewRouter(httpapi.RouterDeps{
		DB:                 db,
		Redis:              redisClient,
		AuthHandler:        handler,
		AuthMW:             mw,
		CORSAllowedOrigins: origins.List(),
	})
	return &cookieEnv{t: t, cfg: cfg, router: router}
}

func (e *cookieEnv) do(req *http.Request) *httptest.ResponseRecorder {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// register creates an email user through the API and returns the response.
func (e *cookieEnv) register() (*httptest.ResponseRecorder, string) {
	e.t.Helper()
	email := "cookie-" + uuid.NewString() + "@example.com"
	e.t.Cleanup(func() { deleteUserByEmail(e.t, email) })
	body, _ := json.Marshal(map[string]string{"email": email, "password": "correct-password", "name": "Cookie User"})
	req := httptest.NewRequest(http.MethodPost, "/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := e.do(req)
	if rec.Code != http.StatusCreated {
		e.t.Fatalf("register: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	return rec, email
}

func deleteUserByEmail(t *testing.T, email string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := postgres.NewPool(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Logf("cleanup connect: %v", err)
		return
	}
	defer db.Close()
	_, _ = db.Exec(ctx, "DELETE FROM users WHERE email = $1", email)
}

func responseCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func requireCookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	c := responseCookie(rec, name)
	if c == nil {
		t.Fatalf("expected Set-Cookie %s, got %v", name, rec.Header().Values("Set-Cookie"))
	}
	return c
}

func requireCleared(t *testing.T, rec *httptest.ResponseRecorder, name string) {
	t.Helper()
	if c := requireCookie(t, rec, name); c.MaxAge >= 0 || c.Value != "" {
		t.Fatalf("expected %s to be cleared, got %+v", name, c)
	}
}

func TestLoginSetsAuthCookies(t *testing.T) {
	e := newCookieEnv(t, auth.CookieConfig{}, auth.OAuthConfig{})
	regRec, email := e.register()

	var reg struct {
		User struct {
			ID    string `json:"id"`
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"user"`
	}
	if err := json.Unmarshal(regRec.Body.Bytes(), &reg); err != nil || reg.User.Email != email || reg.User.ID == "" || reg.User.Name != "Cookie User" {
		t.Fatalf("expected {\"user\": ...} from register, got %s", regRec.Body.String())
	}
	if strings.Contains(regRec.Body.String(), "token") {
		t.Fatalf("register body must not carry tokens: %s", regRec.Body.String())
	}

	body, _ := json.Marshal(map[string]string{"email": email, "password": "correct-password"})
	req := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewReader(body))
	rec := e.do(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "token") || !strings.Contains(rec.Body.String(), `"user":{`) {
		t.Fatalf("login body must be {\"user\": ...} only, got %s", rec.Body.String())
	}

	access := requireCookie(t, rec, auth.AccessTokenCookie)
	refresh := requireCookie(t, rec, auth.RefreshTokenCookie)
	for _, tc := range []struct {
		c      *http.Cookie
		path   string
		maxAge time.Duration
	}{
		{access, "/", e.cfg.JWTAccessTTL},
		{refresh, "/auth", e.cfg.JWTRefreshTTL},
	} {
		if tc.c.Value == "" || tc.c.Path != tc.path || tc.c.MaxAge != int(tc.maxAge.Seconds()) ||
			!tc.c.HttpOnly || tc.c.SameSite != http.SameSiteLaxMode || tc.c.Secure || tc.c.Domain != "" {
			t.Fatalf("unexpected %s cookie attributes: %+v", tc.c.Name, tc.c)
		}
	}

	// Production-style settings flow through to both cookies.
	prod := newCookieEnv(t, auth.CookieConfig{Secure: true, Domain: ".example.com"}, auth.OAuthConfig{})
	prodRec, _ := prod.register()
	for _, name := range []string{auth.AccessTokenCookie, auth.RefreshTokenCookie} {
		if c := requireCookie(t, prodRec, name); !c.Secure || c.Domain != "example.com" {
			t.Fatalf("expected Secure cookie for example.com, got %+v", c)
		}
	}
}

func TestCookieAndBearerAuth(t *testing.T) {
	e := newCookieEnv(t, auth.CookieConfig{}, auth.OAuthConfig{})
	regRec, email := e.register()
	access := requireCookie(t, regRec, auth.AccessTokenCookie)

	me := func(mod func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
		mod(req)
		return e.do(req)
	}

	rec := me(func(r *http.Request) { r.AddCookie(access) })
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), email) {
		t.Fatalf("cookie auth: expected 200 with the user, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = me(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+access.Value) })
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer auth: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = me(func(*http.Request) {})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: expected 401, got %d", rec.Code)
	}
	assertErrorCode(t, rec.Body.String(), "UNAUTHORIZED", "")

	expiredTokens := auth.NewTokenManager(e.cfg.JWTSecret, -time.Minute)
	expired, err := expiredTokens.IssueAccessToken(uuid.New(), email)
	if err != nil {
		t.Fatalf("issue expired token: %v", err)
	}
	rec = me(func(r *http.Request) { r.AddCookie(&http.Cookie{Name: auth.AccessTokenCookie, Value: expired}) })
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired cookie: expected 401, got %d", rec.Code)
	}
	assertErrorCode(t, rec.Body.String(), "TOKEN_EXPIRED", "")
}

func TestRefreshRotatesCookies(t *testing.T) {
	e := newCookieEnv(t, auth.CookieConfig{}, auth.OAuthConfig{})
	regRec, _ := e.register()
	oldRefresh := requireCookie(t, regRec, auth.RefreshTokenCookie)

	refresh := func(c *http.Cookie, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
		if c != nil {
			req.AddCookie(c)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		return e.do(req)
	}

	rec := refresh(oldRefresh, testFrontendOrigin)
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("refresh: expected empty 204, got %d: %s", rec.Code, rec.Body.String())
	}
	newRefresh := requireCookie(t, rec, auth.RefreshTokenCookie)
	newAccess := requireCookie(t, rec, auth.AccessTokenCookie)
	if newRefresh.Value == oldRefresh.Value || newAccess.Value == "" || newRefresh.Path != "/auth" {
		t.Fatalf("expected rotated cookies, got access=%+v refresh=%+v", newAccess, newRefresh)
	}

	// The old refresh token is revoked: 401 and both cookies cleared.
	rec = refresh(oldRefresh, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("reused refresh: expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec.Body.String(), "UNAUTHORIZED", "")
	requireCleared(t, rec, auth.AccessTokenCookie)
	requireCleared(t, rec, auth.RefreshTokenCookie)

	rec = refresh(nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing refresh cookie: expected 401, got %d", rec.Code)
	}
	requireCleared(t, rec, auth.RefreshTokenCookie)

	// A foreign Origin can't use the refresh cookie.
	rec = refresh(newRefresh, "https://evil.example.com")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign origin refresh: expected 403, got %d", rec.Code)
	}
	assertErrorCode(t, rec.Body.String(), "FORBIDDEN", "")

	if rec = refresh(newRefresh, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("rotated refresh: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestLogoutClearsCookies(t *testing.T) {
	e := newCookieEnv(t, auth.CookieConfig{}, auth.OAuthConfig{})
	regRec, _ := e.register()
	refreshCookie := requireCookie(t, regRec, auth.RefreshTokenCookie)

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(refreshCookie)
	rec := e.do(req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	requireCleared(t, rec, auth.AccessTokenCookie)
	requireCleared(t, rec, auth.RefreshTokenCookie)

	// The refresh token was revoked.
	req = httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	req.AddCookie(refreshCookie)
	if rec = e.do(req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("refresh after logout: expected 401, got %d", rec.Code)
	}

	// Logging out without a cookie is still fine.
	if rec = e.do(httptest.NewRequest(http.MethodPost, "/auth/logout", nil)); rec.Code != http.StatusNoContent {
		t.Fatalf("logout without cookie: expected 204, got %d", rec.Code)
	}
}

func TestCORSPreflight(t *testing.T) {
	e := newCookieEnv(t, auth.CookieConfig{}, auth.OAuthConfig{})

	preflight := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodOptions, "/auth/login", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		req.Header.Set("Access-Control-Request-Headers", "content-type")
		return e.do(req)
	}

	rec := preflight(testFrontendOrigin)
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("preflight: expected 2xx, got %d", rec.Code)
	}
	h := rec.Header()
	if h.Get("Access-Control-Allow-Origin") != testFrontendOrigin || h.Get("Access-Control-Allow-Credentials") != "true" ||
		!strings.Contains(h.Get("Access-Control-Allow-Methods"), http.MethodPost) {
		t.Fatalf("preflight: unexpected CORS headers %v", h)
	}

	if got := preflight("https://evil.example.com").Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("foreign origin must not be allowed, got %q", got)
	}

	// Actual requests expose Retry-After so the frontend can read it on 429s.
	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	req.Header.Set("Origin", testFrontendOrigin)
	rec = e.do(req)
	if !strings.Contains(rec.Header().Get("Access-Control-Expose-Headers"), "Retry-After") {
		t.Fatalf("expected Retry-After to be exposed, got %v", rec.Header())
	}
}

// fakeGoogle answers the Google token and userinfo endpoints.
type fakeGoogle struct {
	id, email string
}

func (f fakeGoogle) RoundTrip(r *http.Request) (*http.Response, error) {
	var body string
	switch {
	case r.URL.Host == "oauth2.googleapis.com" && r.URL.Path == "/token":
		body = `{"access_token":"provider-token","token_type":"Bearer","expires_in":3600}`
	case r.URL.Host == "www.googleapis.com" && r.URL.Path == "/oauth2/v2/userinfo":
		b, _ := json.Marshal(map[string]string{"id": f.id, "email": f.email, "name": "OAuth User"})
		body = string(b)
	default:
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: r}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

func TestOAuthCallbackRedirects(t *testing.T) {
	e := newCookieEnv(t, auth.CookieConfig{}, auth.OAuthConfig{
		GoogleClientID:     "test-client",
		GoogleClientSecret: "test-secret",
		GoogleRedirectURL:  "http://localhost:8080/auth/google/callback",
	})
	const stateCookie = "oauth_state_google"

	callback := func(ctx context.Context, path string, state string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		if state != "" {
			req.AddCookie(&http.Cookie{Name: stateCookie, Value: state})
		}
		return e.do(req)
	}
	location := func(rec *httptest.ResponseRecorder) url.Values {
		t.Helper()
		if rec.Code != http.StatusFound {
			t.Fatalf("expected 302, got %d: %s", rec.Code, rec.Body.String())
		}
		loc := rec.Header().Get("Location")
		if !strings.HasPrefix(loc, testCallbackURL+"?") || strings.Contains(loc, "token") {
			t.Fatalf("unexpected redirect %q", loc)
		}
		u, _ := url.Parse(loc)
		return u.Query()
	}

	ctx := context.Background()
	for _, tc := range []struct {
		name, path, state, wantErr string
	}{
		{"github not configured", "/auth/github/callback?code=x&state=s", "s", "SERVICE_UNAVAILABLE"},
		{"user denied", "/auth/google/callback?error=access_denied&state=s", "s", "FORBIDDEN"},
		{"missing state cookie", "/auth/google/callback?code=x&state=s", "", "FORBIDDEN"},
		{"state mismatch", "/auth/google/callback?code=x&state=s", "other", "FORBIDDEN"},
		{"missing code", "/auth/google/callback?state=s", "s", "VALIDATION_ERROR"},
	} {
		rec := callback(ctx, tc.path, tc.state)
		if q := location(rec); q.Get("error") != tc.wantErr || q.Has("status") {
			t.Fatalf("%s: expected ?error=%s, got %v", tc.name, tc.wantErr, q)
		}
		if responseCookie(rec, auth.AccessTokenCookie) != nil {
			t.Fatalf("%s: no auth cookies on failure", tc.name)
		}
	}

	email := "oauth-" + uuid.NewString() + "@example.com"
	t.Cleanup(func() { deleteUserByEmail(t, email) })
	providerCtx := context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: fakeGoogle{id: uuid.NewString(), email: email}})
	rec := callback(providerCtx, "/auth/google/callback?code=good&state=s", "s")
	if q := location(rec); q.Get("status") != "ok" || q.Has("error") {
		t.Fatalf("success: expected ?status=ok, got %v", q)
	}
	access := requireCookie(t, rec, auth.AccessTokenCookie)
	requireCookie(t, rec, auth.RefreshTokenCookie)

	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	req.AddCookie(access)
	if me := e.do(req); me.Code != http.StatusOK || !strings.Contains(me.Body.String(), email) {
		t.Fatalf("cookie from oauth must authenticate, got %d: %s", me.Code, me.Body.String())
	}
}
