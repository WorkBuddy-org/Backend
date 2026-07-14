package auth

import (
	"context"
	"errors"
	"time"

	"github.com/workbuddy/backend/internal/user"
)

type Service struct {
	repo                *Repository
	tokens              *TokenManager
	oauth               *OAuthRegistry
	refreshTTL          time.Duration
	frontendCallbackURL string
}

func NewService(repo *Repository, tokens *TokenManager, oauth *OAuthRegistry, refreshTTL time.Duration, frontendCallbackURL string) *Service {
	return &Service{
		repo:                repo,
		tokens:              tokens,
		oauth:               oauth,
		refreshTTL:          refreshTTL,
		frontendCallbackURL: frontendCallbackURL,
	}
}

type TokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

func (s *Service) OAuthRegistry() *OAuthRegistry {
	return s.oauth
}

func (s *Service) FrontendCallbackURL() string {
	return s.frontendCallbackURL
}

func (s *Service) AuthenticateOAuthUser(ctx context.Context, profile *OAuthProfile) (*user.User, error) {
	existing, err := s.repo.FindUserByOAuth(ctx, profile.Provider, profile.OAuthID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrUserNotFound) {
		return nil, err
	}

	byEmail, err := s.repo.FindUserByEmail(ctx, profile.Email)
	if err == nil {
		if byEmail.OAuthProvider != profile.Provider || byEmail.OAuthID != profile.OAuthID {
			return nil, ErrDuplicateEmail
		}
		return byEmail, nil
	}
	if !errors.Is(err, ErrUserNotFound) {
		return nil, err
	}

	return s.repo.CreateUser(ctx, profile)
}

func (s *Service) IssueTokenPair(ctx context.Context, u *user.User) (*TokenPair, error) {
	accessToken, err := s.tokens.IssueAccessToken(u.ID, u.Email)
	if err != nil {
		return nil, err
	}

	rawRefresh, hash, err := generateRefreshToken()
	if err != nil {
		return nil, err
	}

	expiresAt := time.Now().Add(s.refreshTTL)
	if err := s.repo.CreateRefreshToken(ctx, u.ID, hash, expiresAt); err != nil {
		return nil, err
	}

	return &TokenPair{
		AccessToken:  accessToken,
		RefreshToken: rawRefresh,
		ExpiresIn:    int64(s.tokens.accessTTL.Seconds()),
		TokenType:    "Bearer",
	}, nil
}

func (s *Service) Refresh(ctx context.Context, rawRefresh string) (*TokenPair, error) {
	hash := hashToken(rawRefresh)

	stored, err := s.repo.FindRefreshTokenByHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	if stored.RevokedAt != nil {
		return nil, ErrTokenRevoked
	}
	if time.Now().After(stored.ExpiresAt) {
		return nil, ErrTokenExpired
	}

	if err := s.repo.RevokeRefreshToken(ctx, hash); err != nil {
		return nil, err
	}

	u, err := s.repo.FindUserByID(ctx, stored.UserID)
	if err != nil {
		return nil, err
	}

	return s.IssueTokenPair(ctx, u)
}

func (s *Service) Logout(ctx context.Context, rawRefresh string) error {
	return s.repo.RevokeRefreshToken(ctx, hashToken(rawRefresh))
}

func (s *Service) GetUser(ctx context.Context, userID string) (*user.User, error) {
	id, err := parseUserID(userID)
	if err != nil {
		return nil, err
	}
	return s.repo.FindUserByID(ctx, id)
}
