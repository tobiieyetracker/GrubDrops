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

// MarkClaimed records that a benefit was claimed (possibly externally, e.g.
// via Twitch's own UI). Sets ClaimedAt so ListUnclaimedProgressForAccount
// excludes it and the watcher's pick loop skips it.
func (r *ProgressRecorder) MarkClaimed(ctx context.Context, accountID, benefitID string) error {
	if r == nil || r.Q == nil || accountID == "" || benefitID == "" {
		return nil
	}
	// Preserve existing minutes_watched; only set the claimed timestamp.
	existing, err := r.Q.GetProgress(ctx, gen.GetProgressParams{
		AccountID: accountID,
		BenefitID: benefitID,
	})
	minutes := int64(0)
	if err == nil {
		minutes = existing.MinutesWatched
	}
	return r.Q.UpsertProgress(ctx, gen.UpsertProgressParams{
		AccountID:      accountID,
		BenefitID:      benefitID,
		MinutesWatched: minutes,
		ClaimedAt:      sql.NullInt64{Int64: time.Now().Unix(), Valid: true},
		UpdatedAt:      time.Now().Unix(),
	})
}

// UnclaimedProgress returns all progress rows for the account that have
// not been marked claimed, keyed by benefit ID.
func (r *ProgressRecorder) UnclaimedProgress(ctx context.Context, accountID string) (map[string]int64, error) {
	if r == nil || r.Q == nil || accountID == "" {
		return nil, nil
	}
	// Use a wide time window; the StartsAt/EndsAt filter is for dashboard
	// ranges, not for the watcher's reconciliation.
	rows, err := r.Q.ListUnclaimedProgressForAccount(ctx, gen.ListUnclaimedProgressForAccountParams{
		AccountID: accountID,
		StartsAt:  0,
		EndsAt:    time.Now().Unix() + 86400,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.BenefitID] = row.MinutesWatched
	}
	return out, nil
}
