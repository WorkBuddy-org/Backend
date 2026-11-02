package auth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/workbuddy/backend/internal/user"
	"golang.org/x/oauth2"
)

func TestSplitName(t *testing.T) {
	tests := []struct{ name, first, last string }{
		{"Ada Lovelace", "Ada", "Lovelace"},
		{"Ada", "Ada", ""},
		{"  Ada   King Lovelace ", "Ada", "King Lovelace"},
		{"", "", ""},
	}
	for _, tt := range tests {
		if first, last := splitName(tt.name); first != tt.first || last != tt.last {
			t.Errorf("splitName(%q) = %q, %q; want %q, %q", tt.name, first, last, tt.first, tt.last)
		}
	}
}

// providerResponses answers provider API calls by host and path.
type providerResponses map[string]string

func (p providerResponses) RoundTrip(r *http.Request) (*http.Response, error) {
	status, body := http.StatusNotFound, ""
	if b, ok := p[r.URL.Host+r.URL.Path]; ok {
		status, body = http.StatusOK, b
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

func TestOAuthProfileMapping(t *testing.T) {
	const googleURL, githubURL = "www.googleapis.com/oauth2/v2/userinfo", "api.github.com/user"
	google, github := user.OAuthProviderGoogle, user.OAuthProviderGitHub

	tests := []struct {
		name      string
		provider  user.OAuthProvider
		responses providerResponses
		wantFirst string
		wantLast  string
		wantName  string
		wantPhoto string
	}{
		{
			name:      "google with given and family name",
			provider:  google,
			responses: providerResponses{googleURL: `{"id":"1","email":"a@x.io","name":"Ada L","given_name":"Ada","family_name":"Lovelace","picture":"https://img/a.png"}`},
			wantFirst: "Ada", wantLast: "Lovelace", wantName: "Ada L", wantPhoto: "https://img/a.png",
		},
		{
			name:      "google with only a name and no picture",
			provider:  google,
			responses: providerResponses{googleURL: `{"id":"1","email":"a@x.io","name":"Ada King Lovelace"}`},
			wantFirst: "Ada", wantLast: "King Lovelace", wantName: "Ada King Lovelace",
		},
		{
			name:      "github with a name",
			provider:  github,
			responses: providerResponses{githubURL: `{"id":7,"login":"ada","name":"Ada Lovelace","email":"a@x.io","avatar_url":"https://gh/a.png"}`},
			wantFirst: "Ada", wantLast: "Lovelace", wantName: "Ada Lovelace", wantPhoto: "https://gh/a.png",
		},
		{
			name:      "github with only a login",
			provider:  github,
			responses: providerResponses{githubURL: `{"id":7,"login":"ada-dev","email":"a@x.io"}`},
			wantFirst: "ada-dev", wantName: "ada-dev",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := NewOAuthRegistry(OAuthConfig{
				GoogleClientID: "id", GoogleClientSecret: "secret", GoogleRedirectURL: "http://localhost/cb",
				GitHubClientID: "id", GitHubClientSecret: "secret", GitHubRedirectURL: "http://localhost/cb",
			})
			provider, err := registry.Provider(tt.provider)
			if err != nil {
				t.Fatalf("provider: %v", err)
			}
			ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Transport: tt.responses})

			got, err := provider.FetchProfile(ctx, &oauth2.Token{AccessToken: "t"})
			if err != nil {
				t.Fatalf("fetch profile: %v", err)
			}
			if got.FirstName != tt.wantFirst || got.LastName != tt.wantLast || got.Name != tt.wantName {
				t.Fatalf("unexpected names %+v", got)
			}
			photo := ""
			if got.AvatarURL != nil {
				photo = *got.AvatarURL
			}
			if photo != tt.wantPhoto {
				t.Fatalf("expected avatar %q, got %q", tt.wantPhoto, photo)
			}
		})
	}
}
