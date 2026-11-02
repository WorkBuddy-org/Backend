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

// ProfessionOtherValue is the profession for which profession_other applies.
const ProfessionOtherValue = "other"

// Allowed onboarding values; the database has matching CHECK constraints.
var (
	Goals       = []string{"improve_focus", "share_experience", "improve_skills"}
	Experiences = []string{"lt_1", "1_3", "3_5", "5_plus"}
	Professions = []string{"uxui_designer", "qa", "project_manager", "backend_dev", "frontend_dev", ProfessionOtherValue}
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

	FirstName           string
	LastName            string
	AvatarURL           *string
	Goal                *string
	Experience          *string
	Profession          *string
	ProfessionOther     *string
	OnboardingCompleted bool
	Plan                string
}

// Columns lists the users columns Scan reads, in order.
const Columns = `id, email, name, oauth_provider, oauth_id, role, stack, timezone, avatar_status, created_at, password_hash,
	first_name, last_name, avatar_url, goal, experience, profession, profession_other, onboarding_completed, plan`

// RowScanner is the part of pgx.Row that Scan needs.
type RowScanner interface {
	Scan(dest ...any) error
}

// Scan reads a row selected with Columns. Errors are returned as they are, so
// callers can map pgx.ErrNoRows.
func Scan(row RowScanner) (*User, error) {
	var u User
	var provider string
	err := row.Scan(
		&u.ID, &u.Email, &u.Name, &provider, &u.OAuthID, &u.Role, &u.Stack, &u.Timezone, &u.AvatarStatus, &u.CreatedAt, &u.PasswordHash,
		&u.FirstName, &u.LastName, &u.AvatarURL, &u.Goal, &u.Experience, &u.Profession, &u.ProfessionOther, &u.OnboardingCompleted, &u.Plan,
	)
	if err != nil {
		return nil, err
	}
	u.OAuthProvider = OAuthProvider(provider)
	return &u, nil
}

// DTO is the user object of the API: GET /me, GET /auth/me, and the "user" of
// the register and login responses.
type DTO struct {
	ID                  uuid.UUID `json:"id"`
	Email               string    `json:"email"`
	FirstName           string    `json:"first_name"`
	LastName            string    `json:"last_name"`
	Name                string    `json:"name"`
	AvatarURL           *string   `json:"avatar_url"`
	Plan                string    `json:"plan"`
	Profession          *string   `json:"profession"`
	ProfessionOther     *string   `json:"profession_other"`
	Goal                *string   `json:"goal"`
	Experience          *string   `json:"experience"`
	OnboardingCompleted bool      `json:"onboarding_completed"`
}

func ToDTO(u *User) DTO {
	return DTO{
		ID:                  u.ID,
		Email:               u.Email,
		FirstName:           u.FirstName,
		LastName:            u.LastName,
		Name:                u.Name,
		AvatarURL:           u.AvatarURL,
		Plan:                u.Plan,
		Profession:          u.Profession,
		ProfessionOther:     u.ProfessionOther,
		Goal:                u.Goal,
		Experience:          u.Experience,
		OnboardingCompleted: u.OnboardingCompleted,
	}
}
