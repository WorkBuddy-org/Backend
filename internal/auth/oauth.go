package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/workbuddy/backend/internal/user"
	"golang.org/x/oauth2"
	githuboauth "golang.org/x/oauth2/github"
	googleoauth "golang.org/x/oauth2/google"
)

type OAuthProfile struct {
	Provider user.OAuthProvider
	OAuthID  string
	Email    string
	Name     string
}

type OAuthProvider struct {
	name         user.OAuthProvider
	config       *oauth2.Config
	stateCookie  string
	redirectPath string
}

type OAuthRegistry struct {
	google *OAuthProvider
	github *OAuthProvider
}

func NewOAuthRegistry(cfg OAuthConfig) *OAuthRegistry {
	return &OAuthRegistry{
		google: newGoogleProvider(cfg),
		github: newGitHubProvider(cfg),
	}
}

type OAuthConfig struct {
	GoogleClientID     string
	GoogleClientSecret string
	GoogleRedirectURL  string

	GitHubClientID     string
	GitHubClientSecret string
	GitHubRedirectURL  string
}

func (r *OAuthRegistry) Provider(name user.OAuthProvider) (*OAuthProvider, error) {
	switch name {
	case user.OAuthProviderGoogle:
		if r.google == nil || !r.google.configured() {
			return nil, ErrOAuthNotConfigured
		}
		return r.google, nil
	case user.OAuthProviderGitHub:
		if r.github == nil || !r.github.configured() {
			return nil, ErrOAuthNotConfigured
		}
		return r.github, nil
	default:
		return nil, fmt.Errorf("unsupported oauth provider: %s", name)
	}
}

func newGoogleProvider(cfg OAuthConfig) *OAuthProvider {
	if cfg.GoogleClientID == "" || cfg.GoogleClientSecret == "" || cfg.GoogleRedirectURL == "" {
		return &OAuthProvider{name: user.OAuthProviderGoogle}
	}

	return &OAuthProvider{
		name: user.OAuthProviderGoogle,
		config: &oauth2.Config{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
			RedirectURL:  cfg.GoogleRedirectURL,
			Scopes:       []string{"openid", "email", "profile"},
			Endpoint:     googleoauth.Endpoint,
		},
		stateCookie:  "oauth_state_google",
		redirectPath: "/auth/google",
	}
}

func newGitHubProvider(cfg OAuthConfig) *OAuthProvider {
	if cfg.GitHubClientID == "" || cfg.GitHubClientSecret == "" || cfg.GitHubRedirectURL == "" {
		return &OAuthProvider{name: user.OAuthProviderGitHub}
	}

	return &OAuthProvider{
		name: user.OAuthProviderGitHub,
		config: &oauth2.Config{
			ClientID:     cfg.GitHubClientID,
			ClientSecret: cfg.GitHubClientSecret,
			RedirectURL:  cfg.GitHubRedirectURL,
			Scopes:       []string{"read:user", "user:email"},
			Endpoint:     githuboauth.Endpoint,
		},
		stateCookie:  "oauth_state_github",
		redirectPath: "/auth/github",
	}
}

func (p *OAuthProvider) configured() bool {
	return p != nil && p.config != nil
}

func (p *OAuthProvider) Name() user.OAuthProvider {
	return p.name
}

func (p *OAuthProvider) StateCookieName() string {
	return p.stateCookie
}

func (p *OAuthProvider) AuthCodeURL(state string) string {
	return p.config.AuthCodeURL(state, oauth2.AccessTypeOffline)
}

func (p *OAuthProvider) Exchange(ctx context.Context, code string) (*oauth2.Token, error) {
	return p.config.Exchange(ctx, code)
}

func (p *OAuthProvider) FetchProfile(ctx context.Context, token *oauth2.Token) (*OAuthProfile, error) {
	switch p.name {
	case user.OAuthProviderGoogle:
		return fetchGoogleProfile(ctx, token, p.config)
	case user.OAuthProviderGitHub:
		return fetchGitHubProfile(ctx, token, p.config)
	default:
		return nil, fmt.Errorf("unsupported provider: %s", p.name)
	}
}

func generateOAuthState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate oauth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func fetchGoogleProfile(ctx context.Context, token *oauth2.Token, cfg *oauth2.Config) (*OAuthProfile, error) {
	client := cfg.Client(ctx, token)
	resp, err := client.Get("https://www.googleapis.com/oauth2/v2/userinfo")
	if err != nil {
		return nil, fmt.Errorf("fetch google profile: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("fetch google profile: status %d: %s", resp.StatusCode, string(body))
	}

	var payload struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode google profile: %w", err)
	}
	if payload.ID == "" || payload.Email == "" {
		return nil, fmt.Errorf("google profile missing required fields")
	}

	return &OAuthProfile{
		Provider: user.OAuthProviderGoogle,
		OAuthID:  payload.ID,
		Email:    payload.Email,
		Name:     payload.Name,
	}, nil
}

func fetchGitHubProfile(ctx context.Context, token *oauth2.Token, cfg *oauth2.Config) (*OAuthProfile, error) {
	client := cfg.Client(ctx, token)

	userResp, err := client.Get("https://api.github.com/user")
	if err != nil {
		return nil, fmt.Errorf("fetch github user: %w", err)
	}
	defer userResp.Body.Close()

	if userResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(userResp.Body, 1024))
		return nil, fmt.Errorf("fetch github user: status %d: %s", userResp.StatusCode, string(body))
	}

	var ghUser struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(userResp.Body).Decode(&ghUser); err != nil {
		return nil, fmt.Errorf("decode github user: %w", err)
	}

	email := ghUser.Email
	if email == "" {
		email, err = fetchGitHubPrimaryEmail(client)
		if err != nil {
			return nil, err
		}
	}

	name := ghUser.Name
	if name == "" {
		name = ghUser.Login
	}

	return &OAuthProfile{
		Provider: user.OAuthProviderGitHub,
		OAuthID:  fmt.Sprintf("%d", ghUser.ID),
		Email:    email,
		Name:     name,
	}, nil
}

func fetchGitHubPrimaryEmail(client *http.Client) (string, error) {
	resp, err := client.Get("https://api.github.com/user/emails")
	if err != nil {
		return "", fmt.Errorf("fetch github emails: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("fetch github emails: status %d: %s", resp.StatusCode, string(body))
	}

	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&emails); err != nil {
		return "", fmt.Errorf("decode github emails: %w", err)
	}

	for _, item := range emails {
		if item.Primary && item.Verified {
			return item.Email, nil
		}
	}
	for _, item := range emails {
		if item.Verified {
			return item.Email, nil
		}
	}

	return "", fmt.Errorf("github account has no verified email")
}
