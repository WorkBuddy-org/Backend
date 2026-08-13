package invite

import (
	"time"

	"github.com/google/uuid"
)

type Invite struct {
	ID        uuid.UUID
	Code      string
	CreatedBy uuid.UUID
	UsedBy    *uuid.UUID
	UsedAt    *time.Time
	CreatedAt time.Time
}
