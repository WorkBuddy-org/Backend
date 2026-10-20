package user

import (
	"time"

	"github.com/google/uuid"
)

type OAuthProvider string

const (
	OAuthProviderGoogle OAuthProvider = "google"
	OAuthProviderGitHub OAuthProvider = "github"
	OAuthProviderEmail  OAuthProvider = "email"
)

type User struct {
	ID            uuid.UUID
	Email         string
	Name          string
	OAuthProvider OAuthProvider
	OAuthID       string
	Role          *string
	Stack         *string
	Timezone      *string
	AvatarStatus  string
	CreatedAt     time.Time
	PasswordHash  *string
}
