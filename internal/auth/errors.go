package auth

import (
	"errors"
	"time"
)

var (
	ErrInvalidToken       = errors.New("invalid token")
	ErrTokenExpired       = errors.New("token expired")
	ErrTokenRevoked       = errors.New("token revoked")
	ErrUserNotFound       = errors.New("user not found")
	ErrDuplicateEmail     = errors.New("email already registered with another provider")
	ErrInvalidOAuthState  = errors.New("invalid oauth state")
	ErrOAuthNotConfigured = errors.New("oauth provider not configured")
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrWeakPassword       = errors.New("password must be at least 8 characters")
	ErrInvalidEmail       = errors.New("invalid email address")
	ErrRateLimited        = errors.New("too many login attempts, try again later")
)

// RateLimitedError is ErrRateLimited with how long until the next attempt is
// accepted; errors.Is(err, ErrRateLimited) still holds.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string { return ErrRateLimited.Error() }
func (e *RateLimitedError) Unwrap() error { return ErrRateLimited }
