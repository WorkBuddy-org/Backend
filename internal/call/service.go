package call

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	lkauth "github.com/livekit/protocol/auth"
	"github.com/workbuddy/backend/internal/user"
)

// TokenTTL is how long a LiveKit access token stays valid for joining.
const TokenTTL = time.Hour

// Config holds the LiveKit server settings. The API secret only signs tokens
// and is never returned to clients.
type Config struct {
	URL       string
	APIKey    string
	APISecret string
}

func (c Config) configured() bool {
	return c.URL != "" && c.APIKey != "" && c.APISecret != ""
}

// Participation tells whether a user is currently in a room; *room.Service
// implements it.
type Participation interface {
	HasOpenParticipation(ctx context.Context, roomID, userID uuid.UUID) (bool, error)
}

// Users looks up the caller's display name; *auth.Repository implements it.
type Users interface {
	FindUserByID(ctx context.Context, id uuid.UUID) (*user.User, error)
}

// Token is what a client needs to connect to the room's call.
type Token struct {
	URL   string
	Token string
}

type Service struct {
	cfg   Config
	rooms Participation
	users Users
}

func NewService(cfg Config, rooms Participation, users Users) *Service {
	return &Service{cfg: cfg, rooms: rooms, users: users}
}

// Configured reports whether LiveKit settings are present.
func (s *Service) Configured() bool {
	return s.cfg.configured()
}

// IssueToken mints a LiveKit access token for a user who is currently in the
// room. The LiveKit room name is the room's UUID and the identity is the
// user's UUID.
func (s *Service) IssueToken(ctx context.Context, roomID, userID uuid.UUID) (*Token, error) {
	if !s.cfg.configured() {
		return nil, ErrNotConfigured
	}

	ok, err := s.rooms.HasOpenParticipation(ctx, roomID, userID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotInRoom
	}

	u, err := s.users.FindUserByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("find user: %w", err)
	}

	grant := &lkauth.VideoGrant{RoomJoin: true, Room: roomID.String()}
	grant.SetCanPublish(true)
	grant.SetCanSubscribe(true)

	jwt, err := lkauth.NewAccessToken(s.cfg.APIKey, s.cfg.APISecret).
		SetIdentity(userID.String()).
		SetName(u.Name).
		SetValidFor(TokenTTL).
		SetVideoGrant(grant).
		ToJWT()
	if err != nil {
		return nil, fmt.Errorf("sign livekit token: %w", err)
	}

	return &Token{URL: s.cfg.URL, Token: jwt}, nil
}
