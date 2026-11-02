package room

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SeedRoom describes one room inserted by `migrate seed`.
type SeedRoom struct {
	Title    string
	Type     string
	Tags     []string
	Capacity int
}

// SeedData has no times, so these rooms are always live or open.
var SeedData = []SeedRoom{
	{Title: "Morning Focus Sprint", Type: TypeFocus, Tags: []string{"morning", "quiet"}, Capacity: 4},
	{Title: "Deep Work Session", Type: TypeFocus, Tags: []string{"coding", "quiet"}, Capacity: 6},
	{Title: "Writing Hour", Type: TypeFocus, Tags: []string{"writing", "solo"}, Capacity: 5},
	{Title: "Design Critique", Type: TypeDiscussion, Tags: []string{"design", "feedback"}, Capacity: 8},
	{Title: "Frontend Guild", Type: TypeDiscussion, Tags: []string{"frontend", "code"}, Capacity: 6},
	{Title: "Career Chat", Type: TypeDiscussion, Tags: []string{"career", "mentoring"}, Capacity: 8},
}

// SeedRooms inserts the rooms of SeedData whose title doesn't exist yet and
// returns how many it inserted. Existing rooms are left as they are.
func SeedRooms(ctx context.Context, db *pgxpool.Pool) (int, error) {
	inserted := 0
	for _, rm := range SeedData {
		tag, err := db.Exec(ctx, `
			INSERT INTO rooms (name, type, tags, capacity)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (name) DO NOTHING
		`, rm.Title, rm.Type, rm.Tags, rm.Capacity)
		if err != nil {
			return inserted, fmt.Errorf("seed room %q: %w", rm.Title, err)
		}
		inserted += int(tag.RowsAffected())
	}
	return inserted, nil
}
