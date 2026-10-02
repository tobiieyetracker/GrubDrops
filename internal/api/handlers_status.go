package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/aalejandrofer/grubdrops/internal/watcher"
)

type statusDeps struct {
	snapshots func() []watcher.Snapshot
	startedAt time.Time
}

type statusResponse struct {
	GeneratedAt   time.Time       `json:"generated_at"`
	UptimeSeconds int64           `json:"uptime_seconds"`
	Watchers      []statusWatcher `json:"watchers"`
}

type statusWatcher struct {
	AccountID       string `json:"account_id"`
	State           string `json:"state"`
	CampaignID      string `json:"campaign_id,omitempty"`
	CampaignName    string `json:"campaign_name,omitempty"`
	Game            string `json:"game,omitempty"`
	BenefitID       string `json:"benefit_id,omitempty"`
	BenefitName     string `json:"benefit_name,omitempty"`
	MinutesWatched  int    `json:"minutes_watched"`
	RequiredMinutes int    `json:"required_minutes"`
	ProgressPercent int    `json:"progress_percent"`
	Channel         string `json:"channel,omitempty"`
	StartedAt       string `json:"started_at,omitempty"`
	LastPollAt      string `json:"last_inventory_poll_at,omitempty"`
	LastHeartbeatAt string `json:"last_heartbeat_at,omitempty"`
	LastProgressAt  string `json:"last_progress_at,omitempty"`
}

func (d statusDeps) get(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	startedAt := d.startedAt
	if startedAt.IsZero() {
		startedAt = now
	}

	snapshots := []watcher.Snapshot{}
	if d.snapshots != nil {
		snapshots = d.snapshots()
	}
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].AccountID < snapshots[j].AccountID
	})

	out := statusResponse{
		GeneratedAt:   now.UTC(),
		UptimeSeconds: int64(now.Sub(startedAt).Seconds()),
		Watchers:      make([]statusWatcher, 0, len(snapshots)),
	}
	for _, s := range snapshots {
		item := statusWatcher{
			AccountID:       s.AccountID,
			State:           s.State,
			CampaignID:      s.CampaignID,
			CampaignName:    s.CampaignName,
			Game:            s.CampaignGame,
			BenefitID:       s.BenefitID,
			BenefitName:     s.BenefitName,
			MinutesWatched:  s.MinutesWatched,
			RequiredMinutes: s.RequiredMinutes,
			Channel:         s.Channel,
			StartedAt:       statusTime(s.StartedAt),
			LastPollAt:      statusTime(s.LastPollAt),
			LastHeartbeatAt: statusTime(s.LastHeartbeatAt),
			LastProgressAt:  statusTime(s.LastProgressAt),
		}
		if s.RequiredMinutes > 0 {
			item.ProgressPercent = s.MinutesWatched * 100 / s.RequiredMinutes
			if item.ProgressPercent > 100 {
				item.ProgressPercent = 100
			}
		}
		out.Watchers = append(out.Watchers, item)
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(out)
}

func statusTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
