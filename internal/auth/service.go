package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/workbuddy/backend/internal/user"
	"golang.org/x/crypto/bcrypt"
)

const (
	maxLoginAttempts = 5
	loginAttemptsTTL = 15 * time.Minute
)

type Service struct {
	repo                *Repository
	tokens              *TokenManager
	oauth               *OAuthRegistry
	refreshTTL          time.Duration
	frontendCallbackURL string
	redis               *goredis.Client
}

func NewService(repo *Repository, tokens *TokenManager, oauth *OAuthRegistry, refreshTTL time.Duration, frontendCallbackURL string, redisClient *goredis.Client) *Service {
	return &Service{
		repo:                repo,
		tokens:              tokens,
		oauth:               oauth,
		refreshTTL:          refreshTTL,
		frontendCallbackURL: frontendCallbackURL,
		redis:               redisClient,
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

// RegisterUser creates a new password-based account. Email verification and
// password reset flows are intentionally out of scope pending an email
// provider decision.
func (s *Service) RegisterUser(ctx context.Context, email, password, name string) (*user.User, error) {
	email = normalizeEmail(email)
	if !isValidEmail(email) {
		return nil, ErrInvalidEmail
	}
	if len(password) < 8 {
		return nil, ErrWeakPassword
	}

	_, err := s.repo.FindUserByEmail(ctx, email)
	if err == nil {
		return nil, ErrDuplicateEmail
	}
	if !errors.Is(err, ErrUserNotFound) {
		return nil, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	return s.repo.CreateEmailUser(ctx, email, name, string(hash))
}

// AuthenticatePassword verifies an email+password login. It deliberately
// returns ErrInvalidCredentials for every failure mode (unknown email,
// oauth-only account, wrong password) so the response never leaks which
// part was wrong.
func (s *Service) AuthenticatePassword(ctx context.Context, email, password string) (*user.User, error) {
	email = normalizeEmail(email)

	if err := s.checkLoginRateLimit(ctx, email); err != nil {
		return nil, err
	}

	u, err := s.repo.FindUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}

	if u.PasswordHash == nil {
		return nil, ErrInvalidCredentials
	}

	if err := bcrypt.CompareHashAndPassword([]byte(*u.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}

	s.resetLoginRateLimit(ctx, email)

	return u, nil
}

func (s *Service) checkLoginRateLimit(ctx context.Context, email string) error {
	key := loginAttemptsKey(email)

	count, err := s.redis.Incr(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("check login rate limit: %w", err)
	}
	if count == 1 {
		if err := s.redis.Expire(ctx, key, loginAttemptsTTL).Err(); err != nil {
			return fmt.Errorf("set login rate limit ttl: %w", err)
		}
	}
	if count > maxLoginAttempts {
		return ErrRateLimited
	}

	return nil
}

func (s *Service) resetLoginRateLimit(ctx context.Context, email string) {
	_ = s.redis.Del(ctx, loginAttemptsKey(email)).Err()
}

func loginAttemptsKey(email string) string {
	return "login_attempts:" + email
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func isValidEmail(email string) bool {
	if email == "" || strings.ContainsAny(email, " \t\n\r") {
		return false
	}
	at := strings.Index(email, "@")
	return at > 0 && at < len(email)-1
}
