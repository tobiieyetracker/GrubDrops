package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/aalejandrofer/grubdrops/internal/store/gen"
)

// ProgressRecorder persists the latest inventory observation for each
// account/benefit pair. The watcher calls it after inventory polls, so progress
// remains durable even when nobody is polling the dashboard.
type ProgressRecorder struct {
	Q *gen.Queries
}

func NewProgressRecorder(q *gen.Queries) *ProgressRecorder {
	return &ProgressRecorder{Q: q}
}

func (r *ProgressRecorder) RecordProgress(ctx context.Context, accountID, benefitID string, minutes int) error {
	if r == nil || r.Q == nil || accountID == "" || benefitID == "" || minutes < 0 {
		return nil
	}
	return r.Q.UpsertProgress(ctx, gen.UpsertProgressParams{
		AccountID:      accountID,
		BenefitID:      benefitID,
		MinutesWatched: int64(minutes),
		ClaimedAt:      sql.NullInt64{},
		UpdatedAt:      time.Now().Unix(),
	})
}
