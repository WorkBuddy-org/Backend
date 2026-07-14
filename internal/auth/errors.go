package auth

import "errors"

var (
	ErrInvalidToken       = errors.New("invalid token")
	ErrTokenExpired       = errors.New("token expired")
	ErrTokenRevoked       = errors.New("token revoked")
	ErrUserNotFound       = errors.New("user not found")
	ErrDuplicateEmail     = errors.New("email already registered with another provider")
	ErrInvalidOAuthState  = errors.New("invalid oauth state")
	ErrOAuthNotConfigured = errors.New("oauth provider not configured")
)
