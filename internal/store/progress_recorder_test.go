package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/aalejandrofer/grubdrops/internal/store/gen"
)

func TestProgressRecorder_IsAccountScopedAndStoresZeroObservations(t *testing.T) {
	db := openTest(t)
	q := gen.New(db)
	ctx := context.Background()
	now := time.Now().Unix()
	for _, id := range []string{"acc-a", "acc-b"} {
		_, err := q.CreateAccount(ctx, gen.CreateAccountParams{
			ID: id, Platform: "twitch", DisplayName: id,
			Status: "idle", FingerprintJson: "{}", Enabled: 1,
			CreatedAt: now, UpdatedAt: now,
		})
		require.NoError(t, err)
	}
	require.NoError(t, q.UpsertCampaign(ctx, gen.UpsertCampaignParams{
		ID: "camp-1", Platform: "twitch", Game: "Rust", Name: "Rust",
		StartsAt: now - 3600, EndsAt: now + 3600, Status: "active",
		RawJson: "{}", DiscoveredAt: now, Kind: "drop",
	}))
	require.NoError(t, q.UpsertBenefit(ctx, gen.UpsertBenefitParams{
		ID: "drop-1", CampaignID: "camp-1", Name: "Drop", RequiredMinutes: 60,
	}))

	recorder := NewProgressRecorder(q)
	require.NoError(t, recorder.RecordProgress(ctx, "acc-a", "drop-1", 22))
	// A stale inventory/dashboard snapshot must not roll progress backward.
	require.NoError(t, recorder.RecordProgress(ctx, "acc-a", "drop-1", 0))
	require.NoError(t, recorder.RecordProgress(ctx, "acc-b", "drop-1", 0))

	readMinutes := func(accountID string) int64 {
		t.Helper()
		var minutes int64
		var updatedAt int64
		err := db.QueryRowContext(ctx,
			`SELECT minutes_watched, updated_at FROM progress WHERE account_id = ? AND benefit_id = ?`,
			accountID, "drop-1").Scan(&minutes, &updatedAt)
		require.NoError(t, err)
		require.Positive(t, updatedAt)
		return minutes
	}
	require.EqualValues(t, 22, readMinutes("acc-a"))
	require.EqualValues(t, 0, readMinutes("acc-b"))
	require.NoError(t, recorder.RecordProgress(ctx, "acc-a", "drop-1", 24))
	require.EqualValues(t, 24, readMinutes("acc-a"))
}

func TestProgressRecorder_UnclaimedProgressIncludesCampaignEndingSoon(t *testing.T) {
	db := openTest(t)
	q := gen.New(db)
	ctx := context.Background()
	now := time.Now().Unix()
	_, err := q.CreateAccount(ctx, gen.CreateAccountParams{
		ID: "acc-progress", Platform: "twitch", DisplayName: "acc-progress",
		Status: "idle", FingerprintJson: "{}", Enabled: 1,
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	for _, campaign := range []struct {
		id     string
		endsAt int64
	}{
		{id: "ending-soon", endsAt: now + 1800},
		{id: "already-ended", endsAt: now - 60},
	} {
		require.NoError(t, q.UpsertCampaign(ctx, gen.UpsertCampaignParams{
			ID: campaign.id, Platform: "twitch", Game: "Test", Name: campaign.id,
			StartsAt: now - 3600, EndsAt: campaign.endsAt, Status: "active",
			RawJson: "{}", DiscoveredAt: now, Kind: "drop",
		}))
		require.NoError(t, q.UpsertBenefit(ctx, gen.UpsertBenefitParams{
			ID: campaign.id + "-drop", CampaignID: campaign.id,
			Name: "Drop", RequiredMinutes: 60,
		}))
		require.NoError(t, q.UpsertProgress(ctx, gen.UpsertProgressParams{
			AccountID: "acc-progress", BenefitID: campaign.id + "-drop",
			MinutesWatched: 30, UpdatedAt: now,
		}))
	}

	progress, err := NewProgressRecorder(q).UnclaimedProgress(ctx, "acc-progress")
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"ending-soon-drop": 30}, progress)
}
