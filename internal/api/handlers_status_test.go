package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aalejandrofer/grubdrops/internal/watcher"
)

func TestStatusHandlerSummarizesWatcherSnapshot(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	started := now.Add(-5 * time.Minute)
	snapshot := watcher.Snapshot{
		AccountID:       "acc-a",
		State:           "watching",
		CampaignID:      "camp-rust",
		CampaignName:    "Rust Isles",
		CampaignGame:    "Rust",
		BenefitID:       "drop-door",
		BenefitName:     "Rust Isles Wood Door",
		MinutesWatched:  55,
		RequiredMinutes: 60,
		Channel:         "xchocobars",
		StartedAt:       started,
		LastPollAt:      now.Add(-time.Minute),
		LastHeartbeatAt: now.Add(-20 * time.Second),
		LastProgressAt:  now.Add(-30 * time.Second),
	}
	h := statusDeps{
		startedAt: started,
		snapshots: func() []watcher.Snapshot { return []watcher.Snapshot{snapshot} },
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	h.get(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	var got statusResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Watchers, 1)
	assert.Equal(t, "acc-a", got.Watchers[0].AccountID)
	assert.Equal(t, "watching", got.Watchers[0].State)
	assert.Equal(t, "Rust Isles Wood Door", got.Watchers[0].BenefitName)
	assert.Equal(t, 55, got.Watchers[0].MinutesWatched)
	assert.Equal(t, 60, got.Watchers[0].RequiredMinutes)
	assert.Equal(t, 91, got.Watchers[0].ProgressPercent)
	assert.Equal(t, "xchocobars", got.Watchers[0].Channel)
	assert.Equal(t, now.Add(-20*time.Second).Format(time.RFC3339), got.Watchers[0].LastHeartbeatAt)
	assert.Equal(t, now.Add(-30*time.Second).Format(time.RFC3339), got.Watchers[0].LastProgressAt)
	assert.GreaterOrEqual(t, got.UptimeSeconds, int64(299))
}

func TestStatusRouteRequiresAdmin(t *testing.T) {
	h := NewRouter(Deps{Session: scs.New()})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/login", rec.Header().Get("Location"))
}
