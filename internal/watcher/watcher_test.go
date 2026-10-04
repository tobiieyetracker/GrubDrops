package watcher

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aalejandrofer/grubdrops/internal/platform"
	"github.com/aalejandrofer/grubdrops/internal/platform/platformtest"
)

// TestShouldNotifyProgress_MilestoneSteps verifies progress notifications land
// only on milestone crossings (0% start, each step%, 100%) — never repeatedly
// for the same milestone, which was the Discord spam (Kick stuck at 0/120
// polled every ~60s). Step 0 disables progress notifications entirely.
func TestShouldNotifyProgress_MilestoneSteps(t *testing.T) {
	const req = 120
	w := &Watcher{lastNotifiedMilestone: -1}
	w.cfg.ProgressNotifyStepPct = 50

	if !w.shouldNotifyProgress(0, req) {
		t.Fatal("0% (start) should notify once")
	}
	if w.shouldNotifyProgress(0, req) {
		t.Fatal("repeated 0% must not re-notify (this was the spam)")
	}
	if w.shouldNotifyProgress(30, req) { // 25%
		t.Fatal("25% should not notify with a 50% step")
	}
	if !w.shouldNotifyProgress(60, req) { // 50%
		t.Fatal("50% should notify")
	}
	if w.shouldNotifyProgress(60, req) {
		t.Fatal("repeated 50% must not re-notify")
	}
	if w.shouldNotifyProgress(90, req) { // 75%
		t.Fatal("75% should not notify with a 50% step")
	}
	if !w.shouldNotifyProgress(120, req) { // 100%
		t.Fatal("100% should notify")
	}
	if w.shouldNotifyProgress(130, req) { // capped at 100%
		t.Fatal("past 100% must not re-notify")
	}
}

func TestShouldNotifyProgress_StepZeroDisables(t *testing.T) {
	w := &Watcher{lastNotifiedMilestone: -1}
	w.cfg.ProgressNotifyStepPct = 0
	for _, m := range []int{0, 60, 120} {
		if w.shouldNotifyProgress(m, 120) {
			t.Fatalf("step 0 disables progress notifications; fired at %d", m)
		}
	}
}

type recordingNotifier struct {
	mu     sync.Mutex
	events []string
}

func (r *recordingNotifier) Notify(_ context.Context, ev string, _ map[string]any) error {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	return nil
}

// has reports whether the given event was recorded. Safe to call while
// the watcher goroutine is still emitting events.
func (r *recordingNotifier) has(ev string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e == ev {
			return true
		}
	}
	return false
}

type recordedProgress struct {
	accountID string
	benefitID string
	minutes   int
}

type recordingProgressRecorder struct {
	mu   sync.Mutex
	rows []recordedProgress
}

func (r *recordingProgressRecorder) RecordProgress(_ context.Context, accountID, benefitID string, minutes int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, recordedProgress{accountID: accountID, benefitID: benefitID, minutes: minutes})
	return nil
}

func (r *recordingProgressRecorder) MarkClaimed(_ context.Context, _, _ string) error {
	return nil
}

func (r *recordingProgressRecorder) UnclaimedProgress(_ context.Context, _ string) (map[string]int64, error) {
	return map[string]int64{}, nil
}

func (r *recordingProgressRecorder) snapshot() []recordedProgress {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedProgress(nil), r.rows...)
}

type externallyClaimedProgressRecorder struct {
	marked map[string]bool
}

func (r *externallyClaimedProgressRecorder) RecordProgress(context.Context, string, string, int) error {
	return nil
}

func (r *externallyClaimedProgressRecorder) MarkClaimed(_ context.Context, _, benefitID string) error {
	if r.marked == nil {
		r.marked = map[string]bool{}
	}
	r.marked[benefitID] = true
	return nil
}

func (r *externallyClaimedProgressRecorder) UnclaimedProgress(context.Context, string) (map[string]int64, error) {
	return map[string]int64{"drop1": 12}, nil
}

func TestWatcher_ExternallyClaimedBenefitPersistsSkip(t *testing.T) {
	ctx := context.Background()
	backend := &vanishBackend{MockBackend: platformtest.New()}
	progress := &externallyClaimedProgressRecorder{}
	skips := newRecordingSkipRecorder()
	w := New(Config{
		AccountID:        "acc-external-claim",
		Backend:          backend,
		Session:          platform.Session{AccessToken: "tok"},
		ProgressRecorder: progress,
		SkipRecorder:     skips.recordSkip,
	})

	require.NoError(t, w.pickCampaign(ctx))
	assert.True(t, progress.marked["drop1"], "vanished progress should be marked claimed")
	assert.True(t, skips.skips("acc-external-claim")["drop1"], "vanished benefit skip should survive a restart")
}

type inventoryProgressBackend struct {
	*platformtest.MockBackend
	rows []platform.Progress
}

func (b *inventoryProgressBackend) InventoryProgress(_ context.Context, _ platform.Session) ([]platform.Progress, error) {
	return append([]platform.Progress(nil), b.rows...), nil
}

func TestWatcher_PersistsAllKnownInventoryProgressWithoutDashboard(t *testing.T) {
	ctx := context.Background()
	campaign := platform.Campaign{
		ID: "camp-1", Game: "Rust", Name: "Rust event", Status: "active",
		Benefits: []platform.DropBenefit{
			{ID: "drop-current", CampaignID: "camp-1", Name: "Current", RequiredMinutes: 60},
			{ID: "drop-sibling", CampaignID: "camp-1", Name: "Sibling", RequiredMinutes: 120},
		},
	}
	backend := &inventoryProgressBackend{
		MockBackend: platformtest.New(),
		rows: []platform.Progress{
			{BenefitID: "drop-current", MinutesWatched: 4},
			{BenefitID: "drop-sibling", MinutesWatched: 21},
			// Owned-reward markers aren't timed-drop benefit IDs and must not
			// be sent to the progress table (which has a benefits FK).
			{BenefitID: "reward-marker", MinutesWatched: 60, Claimed: true},
		},
	}
	recorder := &recordingProgressRecorder{}
	session := platform.Session{AccessToken: "tok"}
	stream := platform.Stream{Channel: "rust-stream", DropsEnabled: true}
	handle, err := backend.StartWatch(ctx, session, stream)
	require.NoError(t, err)
	w := New(Config{
		AccountID:         "acc-progress",
		Backend:           backend,
		Session:           session,
		TickInterval:      time.Second,
		HeartbeatInterval: time.Second,
		PriorityMode:      "ending_soonest",
		ProgressRecorder:  recorder,
	})
	w.mu.Lock()
	w.currentCampaign = &campaign
	w.currentBenefit = &campaign.Benefits[0]
	w.currentStream = &stream
	w.handle = &handle
	w.lastDiscovery = []platform.Campaign{campaign}
	w.mu.Unlock()

	require.NoError(t, w.tickWatch(ctx))
	assert.ElementsMatch(t, []recordedProgress{
		{accountID: "acc-progress", benefitID: "drop-current", minutes: 4},
		{accountID: "acc-progress", benefitID: "drop-sibling", minutes: 21},
	}, recorder.snapshot(), "inventory progress should be persisted without a dashboard request")

	snapshot := w.Snapshot()
	assert.Equal(t, 4, snapshot.MinutesWatched)
	assert.False(t, snapshot.LastHeartbeatAt.IsZero())
	assert.False(t, snapshot.LastProgressAt.IsZero())
	assert.False(t, snapshot.LastPollAt.IsZero())
}

func TestWatcher_MinesUntilClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	backend := platformtest.New()
	notif := &recordingNotifier{}

	sess, err := backend.PollDeviceLogin(ctx, platform.DeviceChallenge{})
	require.NoError(t, err)

	w := New(Config{
		AccountID:         "acc1",
		Backend:           backend,
		Session:           sess,
		Notifier:          notif,
		TickInterval:      5 * time.Millisecond,
		HeartbeatInterval: 5 * time.Millisecond,
	})

	// Sleeping is no longer terminal — after claiming the only benefit the
	// watcher enters StateSleeping then re-arms (recheckInterval) so it can
	// pick up newly-active campaigns / freshly-linked accounts without a
	// manual Reload. So Run no longer returns on its own; drive it in a
	// goroutine, wait until it has claimed and reached sleeping, then cancel
	// and assert a clean context-cancelled exit.
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	require.Eventually(t, func() bool {
		return notif.has("claim")
	}, 4*time.Second, 5*time.Millisecond, "watcher should claim the only benefit")

	// Having claimed everything, the watcher parks in the sleeping/recheck
	// cycle rather than exiting. Cancel and confirm a clean shutdown.
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// New() must plumb AllowGame into Session.GameFilter so backends can
// short-circuit non-whitelisted games at the source. Regression guard:
// if someone removes the plumbing the whitelist degrades to a
// watcher-only filter and backends waste bandwidth.
func TestWatcher_New_PropagatesAllowGameToSession(t *testing.T) {
	allow := func(g string) bool { return g == "Rust" }
	w := New(Config{
		AccountID: "acc1",
		Backend:   platformtest.New(),
		Session:   platform.Session{AccessToken: "tok"},
		Notifier:  &recordingNotifier{},
		AllowGame: allow,
	})
	require.NotNil(t, w.cfg.Session.GameFilter)
	assert.True(t, w.cfg.Session.GameFilter("Rust"))
	assert.False(t, w.cfg.Session.GameFilter("Fortnite"))
	assert.Equal(t, "acc1", w.cfg.Session.AccountID)
}

// New() must plumb Config.Games into Session.Games when the session doesn't
// already carry one, so TV-client Twitch sessions (chandisc.go
// listByChannels) can walk a game directory per whitelisted name. Same
// pattern as AllowGame -> GameFilter above.
func TestWatcher_New_PropagatesGamesToSession(t *testing.T) {
	w := New(Config{
		AccountID: "acc1",
		Backend:   platformtest.New(),
		Session:   platform.Session{AccessToken: "tok"},
		Notifier:  &recordingNotifier{},
		Games:     []string{"Rust"},
	})
	assert.Equal(t, []string{"Rust"}, w.cfg.Session.Games)
}

// A session that already carries Games (e.g. refresh() preserved it across a
// token refresh) must not be clobbered by the Config default.
func TestWatcher_New_DoesNotOverrideExistingSessionGames(t *testing.T) {
	w := New(Config{
		AccountID: "acc1",
		Backend:   platformtest.New(),
		Session:   platform.Session{AccessToken: "tok", Games: []string{"Preserved"}},
		Notifier:  &recordingNotifier{},
		Games:     []string{"Rust"},
	})
	assert.Equal(t, []string{"Preserved"}, w.cfg.Session.Games)
}

// recordingPersister captures every batch the watcher pushes to it so we
// can assert the whitelist gate and status filter run BEFORE persistence.
type recordingPersister struct {
	batches [][]platform.Campaign
}

func (r *recordingPersister) PersistCampaigns(_ context.Context, camps []platform.Campaign) error {
	r.batches = append(r.batches, append([]platform.Campaign(nil), camps...))
	return nil
}

// multiStatusBackend mimics a Twitch backend that returns active +
// expired + upcoming campaigns. It exists so the watcher test can verify
// the persister sees ALL statuses while mining only touches the ACTIVE
// one.
type multiStatusBackend struct{ *platformtest.MockBackend }

func (m *multiStatusBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	return []platform.Campaign{
		{ID: "c-active", Game: "Rust", Status: "active", Name: "Active",
			Benefits: []platform.DropBenefit{{ID: "drop1", CampaignID: "c-active", Name: "Drop", RequiredMinutes: 2}}},
		{ID: "c-expired", Game: "Rust", Status: "expired", Name: "Past"},
		{ID: "c-upcoming", Game: "Rust", Status: "upcoming", Name: "Future"},
		{ID: "c-blocked", Game: "Fortnite", Status: "active", Name: "Blocked"},
	}, nil
}

// Watcher must persist EVERY campaign it sees — whitelisted and
// non-whitelisted alike (the /drops Discoverable tab depends on the
// latter). Status filtering happens downstream, not at the persister.
func TestWatcher_PersistsAllWhitelistedStatuses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	rec := &recordingPersister{}
	notif := &recordingNotifier{}
	w := New(Config{
		AccountID:    "acc1",
		Backend:      &multiStatusBackend{platformtest.New()},
		Session:      platform.Session{AccessToken: "tok"},
		Notifier:     notif,
		TickInterval: 5 * time.Millisecond,
		AllowGame:    func(g string) bool { return g == "Rust" },
		Persister:    rec,
	})

	_ = w.Run(ctx)
	require.NotEmpty(t, rec.batches, "persister must be invoked at least once")
	batch := rec.batches[0]

	ids := map[string]string{}
	for _, c := range batch {
		ids[c.ID] = c.Status
	}
	assert.Equal(t, "active", ids["c-active"])
	assert.Equal(t, "expired", ids["c-expired"])
	assert.Equal(t, "upcoming", ids["c-upcoming"])
	// Non-whitelisted campaigns are persisted as shell rows so the
	// /drops Discoverable tab can list them; the watcher's mining
	// loop still skips them via AllowGame.
	assert.Equal(t, "active", ids["c-blocked"], "non-whitelisted campaign must reach persister for Discoverable")
}

// rewardOnlyBackend returns a single ACTIVE Twitch campaign of kind="reward"
// with empty Benefits. The watcher MUST skip it from the mining loop AND
// dispatch the reward reaper if the backend implements RewardClaimer.
type rewardOnlyBackend struct {
	*platformtest.MockBackend
	calls    int
	games    []string
	claimRet []platform.ClaimedReward
}

func (r *rewardOnlyBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	return []platform.Campaign{
		{ID: "minecraft|builder-cape", Platform: "twitch", Game: "Minecraft",
			Status: "active", Kind: "reward", AccountLinked: true, Name: "Builder Cape"},
	}, nil
}

func (r *rewardOnlyBackend) ClaimRewards(_ context.Context, _ platform.Session, allowed []string) ([]platform.ClaimedReward, error) {
	r.calls++
	r.games = append([]string{}, allowed...)
	return r.claimRet, nil
}

// vanishBackend simulates a Twitch code-style drop: progress accrues
// for a few ticks, then the benefit silently disappears from
// dropCampaignsInProgress (Twitch behaviour when a code drop is issued
// or campaign expires mid-watch). The watcher must detect the vanish
// and re-enter PickCampaign instead of mining a ghost benefit forever
// (B2.5).
type vanishBackend struct {
	*platformtest.MockBackend
	mu         sync.Mutex
	inv        []platform.Progress
	stopCalled int
}

func (v *vanishBackend) InventoryProgress(_ context.Context, _ platform.Session) ([]platform.Progress, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]platform.Progress, len(v.inv))
	copy(out, v.inv)
	return out, nil
}

func (v *vanishBackend) setProgress(p []platform.Progress) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.inv = p
}

func (v *vanishBackend) StopWatch(_ context.Context, _ platform.WatchHandle) error {
	v.mu.Lock()
	v.stopCalled++
	v.mu.Unlock()
	return nil
}

func (v *vanishBackend) stops() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.stopCalled
}

// vanishBackend.ListActiveCampaigns returns one drop benefit with a
// high RequiredMinutes so the watcher stays in tickWatch (never
// transitions to Claiming on its own).
func (v *vanishBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	return []platform.Campaign{{
		ID: "camp1", Game: "Rust", Name: "Rust Camp", Status: "active", AccountLinked: true,
		Benefits: []platform.DropBenefit{{ID: "drop1", CampaignID: "camp1", Name: "Drop", RequiredMinutes: 999}},
	}}, nil
}

// TestWatcher_VanishDetect: progress 1/999 then 2/999, then benefit
// disappears from inventory for 3 consecutive ticks. Watcher must call
// StopWatch and re-enter PickCampaign rather than continue heartbeating
// against a benefit that will never finish.
func TestWatcher_VanishDetect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	backend := &vanishBackend{MockBackend: platformtest.New()}
	backend.setProgress([]platform.Progress{{BenefitID: "drop1", MinutesWatched: 2}})

	notif := &recordingNotifier{}
	w := New(Config{
		AccountID:         "acc-vanish",
		Backend:           backend,
		Session:           platform.Session{AccessToken: "tok"},
		Notifier:          notif,
		TickInterval:      2 * time.Millisecond,
		HeartbeatInterval: 2 * time.Millisecond,
	})

	go func() { _ = w.Run(ctx) }()

	// Wait for watcher to reach watching state and accumulate at least
	// one matched-progress tick.
	require.Eventually(t, func() bool {
		s := w.Snapshot()
		return s.MinutesWatched >= 1
	}, time.Second, 5*time.Millisecond, "watcher never observed progress")

	// Now vanish the benefit. Watcher should StopWatch + reset within
	// vanishThreshold ticks.
	backend.setProgress(nil)
	require.Eventually(t, func() bool {
		return backend.stops() >= 1
	}, time.Second, 5*time.Millisecond, "watcher did not StopWatch after benefit vanished")
}

// ghostSkipBackend serves a campaign with two benefits but reports an
// EMPTY in-progress inventory, so the watcher never sees either drop
// enroll and must ghost-skip both after synthSkipThreshold ticks.
type ghostSkipBackend struct {
	*platformtest.MockBackend
	mu         sync.Mutex
	stopCalled int
}

func (g *ghostSkipBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	return []platform.Campaign{{
		ID: "gcamp", Game: "GhostGame", Name: "Ghost Camp", Status: "active", AccountLinked: true,
		Benefits: []platform.DropBenefit{
			{ID: "ghost1", CampaignID: "gcamp", Name: "Ghost One", RequiredMinutes: 60},
			{ID: "ghost2", CampaignID: "gcamp", Name: "Ghost Two", RequiredMinutes: 120},
		},
	}}, nil
}

func (g *ghostSkipBackend) InventoryProgress(_ context.Context, _ platform.Session) ([]platform.Progress, error) {
	return nil, nil // never reports any in-progress drop → ghost-skip fires
}

func (g *ghostSkipBackend) StopWatch(_ context.Context, _ platform.WatchHandle) error {
	g.mu.Lock()
	g.stopCalled++
	g.mu.Unlock()
	return nil
}

func (g *ghostSkipBackend) stops() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stopCalled
}

// recordingSkipRecorder captures every (accountID, benefitID) the watcher
// records as a ghost-skip, so a test can assert persistence happened and
// re-seed a fresh watcher from the same set.
type recordingSkipRecorder struct {
	mu   sync.Mutex
	seen map[string]bool // key = accountID + ":" + benefitID
}

func newRecordingSkipRecorder() *recordingSkipRecorder {
	return &recordingSkipRecorder{seen: map[string]bool{}}
}

func (r *recordingSkipRecorder) recordSkip(accountID, benefitID string) error {
	r.mu.Lock()
	// Match the kv convention: benefitID + ":" + accountID (see
	// loadSkipOverrides / CollectOverridePrefix).
	r.seen[benefitID+":"+accountID] = true
	r.mu.Unlock()
	return nil
}

func (r *recordingSkipRecorder) skips(accountID string) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]bool{}
	for k := range r.seen {
		// k = benefitID + ":" + accountID
		benefitID, acc, ok := strings.Cut(k, ":")
		if ok && acc == accountID {
			out[benefitID] = true
		}
	}
	return out
}

// TestWatcher_GhostSkip_PersistsAcrossRestart verifies that a benefit
// the watcher ghost-skips is (a) recorded via SkipRecorder and (b) NOT
// re-picked after a simulated restart — a fresh watcher seeded with the
// recorded skips via PersistedSkips must skip both ghost benefits
// immediately rather than burning synthSkipThreshold ticks each.
func TestWatcher_GhostSkip_PersistsAcrossRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	const accID = "acc-ghost"
	rec := newRecordingSkipRecorder()
	backend := &ghostSkipBackend{MockBackend: platformtest.New()}

	// Phase 1: drive the first watcher. It should pick a ghost benefit,
	// watch for synthSkipThreshold ticks, then ghost-skip it and record it.
	w1 := New(Config{
		AccountID:         accID,
		Backend:           backend,
		Session:           platform.Session{AccessToken: "tok"},
		Notifier:          &recordingNotifier{},
		TickInterval:      2 * time.Millisecond,
		HeartbeatInterval: 2 * time.Millisecond,
		SkipRecorder:      rec.recordSkip,
	})
	go func() { _ = w1.Run(ctx) }()

	// Wait for BOTH ghost benefits to be recorded as skipped. The watcher
	// picks ghost1, watches synthSkipThreshold ticks, skips+records it,
	// then picks ghost2 and repeats — so we need to wait for the full
	// cycle before snapshotting the skips for the restart simulation.
	require.Eventually(t, func() bool {
		return len(rec.skips(accID)) >= 2
	}, 3*time.Second, 5*time.Millisecond,
		"watcher never recorded both ghost-skips")

	// Phase 2: simulate a restart. Build a FRESH watcher (new skippedBenefits
	// map) seeded with the persisted skips. It must NOT pick either ghost
	// benefit — pickCampaign's skippedBenefits filter should skip them
	// immediately because PersistedSkips pre-seeded the set at New().
	cancel() // stop w1 before building w2 to avoid shared-backend races
	ctx2, cancel2 := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel2()

	backend2 := &ghostSkipBackend{MockBackend: platformtest.New()}
	skipsSoFar := rec.skips(accID)
	w2 := New(Config{
		AccountID:         accID,
		Backend:           backend2,
		Session:           platform.Session{AccessToken: "tok"},
		Notifier:          &recordingNotifier{},
		TickInterval:      2 * time.Millisecond,
		HeartbeatInterval: 2 * time.Millisecond,
		PersistedSkips: func(accountID string) (map[string]bool, error) {
			if accountID == accID {
				return skipsSoFar, nil
			}
			return map[string]bool{}, nil
		},
	})
	go func() { _ = w2.Run(ctx2) }()

	// Give the watcher a few ticks to run a discovery + pick cycle. If the
	// persisted skips are correctly seeded, it should NOT start watching
	// either ghost benefit (which would be observable as a StateWatching
	// transition or a StopWatch call after a ghost-skip).
	time.Sleep(200 * time.Millisecond)

	// The pre-seeded watcher must NOT have started watching — both benefits
	// are in skippedBenefits, so pickCampaign skips them and the watcher
	// sleeps with no current benefit. backend2.stops() stays 0 (no ghost-skip
	// path fired because nothing was ever picked).
	assert.Equal(t, 0, backend2.stops(),
		"fresh watcher seeded with persisted skips must not watch ghost benefits")

	// And both ghost benefits should be in the pre-seeded set.
	assert.Contains(t, skipsSoFar, "ghost1", "ghost1 should have been recorded as skipped")
}

// excludeBackend returns two ACTIVE Rust campaigns; the watcher must
// skip the one whose Game is in the ExcludeGame predicate (P3).
type excludeBackend struct {
	*platformtest.MockBackend
	mu     sync.Mutex
	picked string
}

func (e *excludeBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	return []platform.Campaign{
		{ID: "skip", Game: "Skipme", Status: "active", AccountLinked: true,
			Benefits: []platform.DropBenefit{{ID: "d_skip", RequiredMinutes: 2}}},
		{ID: "ok", Game: "Rust", Status: "active", AccountLinked: true,
			Benefits: []platform.DropBenefit{{ID: "d_ok", RequiredMinutes: 2}}},
	}, nil
}

func (e *excludeBackend) ListEligibleChannels(_ context.Context, _ platform.Session, c platform.Campaign) ([]platform.Stream, error) {
	e.mu.Lock()
	if e.picked == "" {
		e.picked = c.ID
	}
	e.mu.Unlock()
	return []platform.Stream{{Channel: "streamer"}}, nil
}

func (e *excludeBackend) firstPicked() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.picked
}

// TestWatcher_ExcludeGame skips campaigns whose game matches the
// exclude predicate (P3).
func TestWatcher_ExcludeGame(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	backend := &excludeBackend{MockBackend: platformtest.New()}
	w := New(Config{
		AccountID:    "acc_excl",
		Backend:      backend,
		Session:      platform.Session{AccessToken: "tok"},
		Notifier:     &recordingNotifier{},
		TickInterval: 2 * time.Millisecond,
		AllowGame:    func(g string) bool { return true },
		ExcludeGame:  func(g string) bool { return g == "Skipme" },
	})

	go func() { _ = w.Run(ctx) }()
	require.Eventually(t, func() bool { return backend.firstPicked() != "" },
		time.Second, 5*time.Millisecond, "watcher never picked any campaign")
	assert.Equal(t, "ok", backend.firstPicked(), "ExcludeGame must skip Skipme")
}

// lowAvblBackend returns multiple ACTIVE campaigns with varying
// AllowedChannelCount values so we can verify PriorityMode
// "low_avbl_first" picks the scarcest campaign first (P1).
type lowAvblBackend struct {
	*platformtest.MockBackend
	picked string
	mu     sync.Mutex
}

func (l *lowAvblBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	return []platform.Campaign{
		{ID: "wide", Game: "Rust", Status: "active", AccountLinked: true,
			AllowedChannelCount: 50,
			Benefits:            []platform.DropBenefit{{ID: "drop_wide", CampaignID: "wide", RequiredMinutes: 2}}},
		{ID: "narrow", Game: "Rust", Status: "active", AccountLinked: true,
			AllowedChannelCount: 3,
			Benefits:            []platform.DropBenefit{{ID: "drop_narrow", CampaignID: "narrow", RequiredMinutes: 2}}},
		{ID: "any", Game: "Rust", Status: "active", AccountLinked: true,
			AllowedChannelCount: 0, // unrestricted — sorted last
			Benefits:            []platform.DropBenefit{{ID: "drop_any", CampaignID: "any", RequiredMinutes: 2}}},
	}, nil
}

func (l *lowAvblBackend) ListEligibleChannels(_ context.Context, _ platform.Session, c platform.Campaign) ([]platform.Stream, error) {
	l.mu.Lock()
	if l.picked == "" {
		l.picked = c.ID
	}
	l.mu.Unlock()
	return []platform.Stream{{Channel: "streamer"}}, nil
}

func (l *lowAvblBackend) firstPicked() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.picked
}

// TestWatcher_LowAvblFirst_PicksScarceFirst (P1)
func TestWatcher_LowAvblFirst_PicksScarceFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	backend := &lowAvblBackend{MockBackend: platformtest.New()}
	w := New(Config{
		AccountID:    "acc_low",
		Backend:      backend,
		Session:      platform.Session{AccessToken: "tok"},
		Notifier:     &recordingNotifier{},
		TickInterval: 2 * time.Millisecond,
		AllowGame:    func(g string) bool { return g == "Rust" },
		PriorityMode: "low_avbl_first",
	})

	go func() { _ = w.Run(ctx) }()

	require.Eventually(t, func() bool {
		return backend.firstPicked() != ""
	}, time.Second, 5*time.Millisecond, "watcher never reached pickStream")

	assert.Equal(t, "narrow", backend.firstPicked(),
		"low_avbl_first must prefer scarcest campaign (narrow), not wide or unrestricted")
}

// offlineFirstBackend returns two ACTIVE campaigns of the same game.
// The higher-priority one ("dead", listed first) has NO live channels;
// the lower-priority one ("live") does. The watcher must skip the dead
// campaign and advance to the live one instead of sleeping forever on
// the highest-priority pick (esports-channel trap).
type offlineFirstBackend struct {
	*platformtest.MockBackend
	mu     sync.Mutex
	picked string
}

func (o *offlineFirstBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	return []platform.Campaign{
		{ID: "dead", Game: "Rust", Status: "active", AccountLinked: true,
			Benefits: []platform.DropBenefit{{ID: "d_dead", CampaignID: "dead", RequiredMinutes: 2}}},
		{ID: "live", Game: "Rust", Status: "active", AccountLinked: true,
			Benefits: []platform.DropBenefit{{ID: "d_live", CampaignID: "live", RequiredMinutes: 2}}},
	}, nil
}

func (o *offlineFirstBackend) ListEligibleChannels(_ context.Context, _ platform.Session, c platform.Campaign) ([]platform.Stream, error) {
	if c.ID == "dead" {
		return nil, nil // no live broadcaster
	}
	o.mu.Lock()
	if o.picked == "" {
		o.picked = c.ID
	}
	o.mu.Unlock()
	return []platform.Stream{{Channel: "streamer"}}, nil
}

func (o *offlineFirstBackend) firstPicked() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.picked
}

// TestWatcher_SkipsOfflineCampaignToNextLive: when the top-priority
// campaign has no live channel, the watcher must advance to the next
// eligible campaign that does — not get stuck re-picking the dead one.
func TestWatcher_SkipsOfflineCampaignToNextLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	backend := &offlineFirstBackend{MockBackend: platformtest.New()}
	w := New(Config{
		AccountID:    "acc_offline",
		Backend:      backend,
		Session:      platform.Session{AccessToken: "tok"},
		Notifier:     &recordingNotifier{},
		TickInterval: 2 * time.Millisecond,
		AllowGame:    func(g string) bool { return g == "Rust" },
	})

	go func() { _ = w.Run(ctx) }()
	require.Eventually(t, func() bool { return backend.firstPicked() != "" },
		time.Second, 5*time.Millisecond, "watcher never advanced to a live campaign")
	assert.Equal(t, "live", backend.firstPicked(),
		"watcher must skip the offline 'dead' campaign and mine the live one")
}

// restrictedFirstBackend returns an OPEN campaign (no AllowedChannels)
// ahead of a channel-RESTRICTED one. On Kick the open campaign accrues
// passively on any participating channel, so the watcher must actively
// mine the restricted one first.
type restrictedFirstBackend struct {
	*platformtest.MockBackend
	mu     sync.Mutex
	picked string
}

func (r *restrictedFirstBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	return []platform.Campaign{
		{ID: "open", Game: "Rust", Status: "active", AccountLinked: true,
			Benefits: []platform.DropBenefit{{ID: "d_open", CampaignID: "open", RequiredMinutes: 2}}},
		{ID: "team", Game: "Rust", Status: "active", AccountLinked: true,
			AllowedChannels: []string{"teamchan"},
			Benefits:        []platform.DropBenefit{{ID: "d_team", CampaignID: "team", RequiredMinutes: 2}}},
	}, nil
}

func (r *restrictedFirstBackend) ListEligibleChannels(_ context.Context, _ platform.Session, c platform.Campaign) ([]platform.Stream, error) {
	r.mu.Lock()
	if r.picked == "" {
		r.picked = c.ID
	}
	r.mu.Unlock()
	return []platform.Stream{{Channel: "streamer"}}, nil
}

func (r *restrictedFirstBackend) firstPicked() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.picked
}

// TestWatcher_KickPicksRestrictedCampaignFirst: on Kick, open campaigns
// (empty AllowedChannels) accrue watch-time passively while any
// participating channel is watched, so the watcher must spend its watch
// slot on channel-restricted (team) campaigns first.
func TestWatcher_KickPicksRestrictedCampaignFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	backend := &restrictedFirstBackend{MockBackend: platformtest.New()}
	w := New(Config{
		AccountID:    "acc_kick_restricted",
		Platform:     "kick",
		Backend:      backend,
		Session:      platform.Session{AccessToken: "tok"},
		Notifier:     &recordingNotifier{},
		TickInterval: 2 * time.Millisecond,
		AllowGame:    func(g string) bool { return g == "Rust" },
	})

	go func() { _ = w.Run(ctx) }()
	require.Eventually(t, func() bool { return backend.firstPicked() != "" },
		time.Second, 5*time.Millisecond, "watcher never picked a campaign")
	assert.Equal(t, "team", backend.firstPicked(),
		"kick watcher must mine the channel-restricted campaign first; open ones accrue passively")
}

// TestWatcher_TwitchPicksRestrictedCampaignFirst: the restricted-first
// partition applies to Twitch too — restricted campaigns are limited to
// specific broadcasters live only in narrow windows, so they're mined ahead
// of open campaigns (which can be done from any channel for the game anytime).
func TestWatcher_TwitchPicksRestrictedCampaignFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	backend := &restrictedFirstBackend{MockBackend: platformtest.New()}
	w := New(Config{
		AccountID:    "acc_twitch_order",
		Platform:     "twitch",
		Backend:      backend,
		Session:      platform.Session{AccessToken: "tok"},
		Notifier:     &recordingNotifier{},
		TickInterval: 2 * time.Millisecond,
		AllowGame:    func(g string) bool { return g == "Rust" },
	})

	go func() { _ = w.Run(ctx) }()
	require.Eventually(t, func() bool { return backend.firstPicked() != "" },
		time.Second, 5*time.Millisecond, "watcher never picked a campaign")
	assert.Equal(t, "team", backend.firstPicked(),
		"twitch watcher must mine the channel-restricted campaign first")
}

// pubsubAwareBackend captures hook registration calls so we can verify
// the watcher registers per-account PubSub hooks at construction time.
type pubsubAwareBackend struct {
	*platformtest.MockBackend
	hooks   map[string]platform.PubSubHooks
	subs    []string
	unsubs  []string
	hooksMu sync.Mutex
}

func (p *pubsubAwareBackend) SetAccountPubSubHooks(accountID string, h platform.PubSubHooks) {
	p.hooksMu.Lock()
	defer p.hooksMu.Unlock()
	if p.hooks == nil {
		p.hooks = map[string]platform.PubSubHooks{}
	}
	p.hooks[accountID] = h
}

func (p *pubsubAwareBackend) SubscribeChannel(_, channelID string) {
	p.hooksMu.Lock()
	defer p.hooksMu.Unlock()
	p.subs = append(p.subs, channelID)
}

func (p *pubsubAwareBackend) UnsubscribeChannel(_, channelID string) {
	p.hooksMu.Lock()
	defer p.hooksMu.Unlock()
	p.unsubs = append(p.unsubs, channelID)
}

// TestWatcher_RegistersPubSubHooks: the watcher constructor must call
// SetAccountPubSubHooks so the backend's lazy PubSub bootstrap picks
// up the watcher's callbacks. Without this F1 doesn't fire.
func TestWatcher_RegistersPubSubHooks(t *testing.T) {
	backend := &pubsubAwareBackend{MockBackend: platformtest.New()}
	w := New(Config{
		AccountID: "acc_hooks",
		Backend:   backend,
		Session:   platform.Session{AccessToken: "tok"},
		Notifier:  &recordingNotifier{},
	})
	_ = w

	backend.hooksMu.Lock()
	defer backend.hooksMu.Unlock()
	hooks, ok := backend.hooks["acc_hooks"]
	require.True(t, ok, "hooks not registered for account")
	assert.NotNil(t, hooks.OnDropProgress)
	assert.NotNil(t, hooks.OnDropClaim)
	assert.NotNil(t, hooks.OnStreamDown)
	assert.NotNil(t, hooks.OnStreamUp)
}

// TestWatcher_PubSubHooks_UpdateProgress: the OnDropProgress hook must
// update lastProgressMin so the dashboard reflects real-time progress
// between inventory polls.
func TestWatcher_PubSubHooks_UpdateProgress(t *testing.T) {
	backend := &pubsubAwareBackend{MockBackend: platformtest.New()}
	w := New(Config{
		AccountID: "acc1",
		Backend:   backend,
		Session:   platform.Session{AccessToken: "tok"},
		Notifier:  &recordingNotifier{},
	})

	// Simulate watcher having picked a benefit.
	w.mu.Lock()
	w.currentBenefit = &platform.DropBenefit{ID: "drop1", RequiredMinutes: 10}
	w.mu.Unlock()

	backend.hooksMu.Lock()
	hooks := backend.hooks["acc1"]
	backend.hooksMu.Unlock()
	require.NotNil(t, hooks.OnDropProgress)

	hooks.OnDropProgress("drop1", 7, 10)

	snap := w.Snapshot()
	assert.Equal(t, 7, snap.MinutesWatched, "OnDropProgress must advance lastProgressMin")
}

// Watcher must fire the reward reaper when a whitelisted kind=reward
// campaign is discovered and the backend implements RewardClaimer.
func TestWatcher_RewardReaperFires(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	rec := &recordingPersister{}
	notif := &recordingNotifier{}
	backend := &rewardOnlyBackend{
		MockBackend: platformtest.New(),
		claimRet:    []platform.ClaimedReward{{Game: "Minecraft", Title: "Builder Cape"}},
	}
	w := New(Config{
		AccountID:    "acc1",
		Backend:      backend,
		Session:      platform.Session{AccessToken: "tok"},
		Notifier:     notif,
		TickInterval: 5 * time.Millisecond,
		AllowGame:    func(g string) bool { return g == "Minecraft" },
		Persister:    rec,
	})

	_ = w.Run(ctx)
	assert.GreaterOrEqual(t, backend.calls, 1, "ClaimRewards must be invoked at least once")
	assert.Contains(t, backend.games, "Minecraft", "reaper must scope claim to whitelisted game")
}

// errHeartbeatBackend starts a watch successfully, then fails every
// Heartbeat with a TRANSIENT (non-cancel) error — simulating a Kick WS
// presence loop that dies mid-watch. tickWatch surfaces that as a step
// error, so Run takes its transient-error → PickCampaign branch. Each
// StartWatch hands back a distinct live handle; StopWatch records how many
// of those handles the watcher actually tore down.
type errHeartbeatBackend struct {
	*platformtest.MockBackend
	mu          sync.Mutex
	startCalled int
	stopCalled  int
}

func (e *errHeartbeatBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	return []platform.Campaign{{
		ID: "camp1", Game: "Rust", Name: "Rust Camp", Status: "active", AccountLinked: true,
		Benefits: []platform.DropBenefit{{ID: "drop1", CampaignID: "camp1", Name: "Drop", RequiredMinutes: 9999}},
	}}, nil
}

func (e *errHeartbeatBackend) ListEligibleChannels(_ context.Context, _ platform.Session, _ platform.Campaign) ([]platform.Stream, error) {
	return []platform.Stream{{Channel: "chan1", DropsEnabled: true}}, nil
}

func (e *errHeartbeatBackend) StartWatch(_ context.Context, _ platform.Session, s platform.Stream) (platform.WatchHandle, error) {
	e.mu.Lock()
	e.startCalled++
	n := e.startCalled
	e.mu.Unlock()
	// Non-nil Internal so the handle looks like a real, live watch the
	// watcher is obligated to stop (mirrors the WS path's *kickWSWatch).
	return platform.WatchHandle{Channel: s.Channel, Internal: n}, nil
}

func (e *errHeartbeatBackend) Heartbeat(_ context.Context, _ platform.WatchHandle) error {
	return errors.New("ws presence loop died")
}

func (e *errHeartbeatBackend) StopWatch(_ context.Context, _ platform.WatchHandle) error {
	e.mu.Lock()
	e.stopCalled++
	e.mu.Unlock()
	return nil
}

func (e *errHeartbeatBackend) starts() int { e.mu.Lock(); defer e.mu.Unlock(); return e.startCalled }
func (e *errHeartbeatBackend) stops() int  { e.mu.Lock(); defer e.mu.Unlock(); return e.stopCalled }

// TestWatcher_StopsWatchOnTransientError: when a watch is live and the tick
// fails with a transient error (e.g. the Kick WS presence loop died), Run
// must STOP that watch before falling back to PickCampaign. Otherwise the
// pure-WS path leaks the background presence goroutine (it runs on its own
// context, only stoppable via the handle we're about to discard) and the
// next StartWatch opens a SECOND presence for the same account — the server
// credits one watch per account, so the new one accrues nothing and the
// watcher death-loops join→bounce→pick_campaign forever.
func TestWatcher_StopsWatchOnTransientError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	backend := &errHeartbeatBackend{MockBackend: platformtest.New()}
	w := New(Config{
		AccountID:         "acc-err",
		Backend:           backend,
		Session:           platform.Session{AccessToken: "tok"},
		Notifier:          &recordingNotifier{},
		TickInterval:      2 * time.Millisecond,
		HeartbeatInterval: 2 * time.Millisecond,
	})

	go func() { _ = w.Run(ctx) }()

	// The watcher must reach the watch and then fail the heartbeat tick.
	require.Eventually(t, func() bool {
		return backend.starts() >= 1
	}, time.Second, 2*time.Millisecond, "watcher never started a watch")

	// Every started watch must be torn down: a live handle abandoned on the
	// error path is a leaked WS presence.
	require.Eventually(t, func() bool {
		return backend.stops() >= 1
	}, time.Second, 2*time.Millisecond, "watcher abandoned a live watch on the transient-error path (leaked WS presence)")
}

// freezeBackend simulates a server-side stall: the current benefit is
// ALWAYS present in inventory (so the vanish-detect path never fires) but
// its MinutesWatched is controllable. When held flat it must trip the
// freeze detector; when advancing it must not. RequiredMinutes is high so
// the watcher never reaches the claim path on its own.
type freezeBackend struct {
	*platformtest.MockBackend
	mu         sync.Mutex
	minutes    int
	advance    bool // when true, each InventoryProgress poll bumps minutes
	stopCalled int
}

func (f *freezeBackend) InventoryProgress(_ context.Context, _ platform.Session) ([]platform.Progress, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.advance {
		f.minutes++
	}
	return []platform.Progress{{BenefitID: "drop1", MinutesWatched: f.minutes}}, nil
}

func (f *freezeBackend) StopWatch(_ context.Context, _ platform.WatchHandle) error {
	f.mu.Lock()
	f.stopCalled++
	f.mu.Unlock()
	return nil
}

func (f *freezeBackend) stops() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopCalled
}

func (f *freezeBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	return []platform.Campaign{{
		ID: "camp1", Game: "Rust", Name: "Rust Camp", Status: "active", AccountLinked: true,
		Benefits: []platform.DropBenefit{{ID: "drop1", CampaignID: "camp1", Name: "Drop", RequiredMinutes: 9999}},
	}}, nil
}

// TestWatcher_FreezeDetect_RotatesOnStalledMinutes: the benefit stays
// present in inventory but its MinutesWatched never advances. After
// freezeThreshold consecutive flat polls the watcher must rotate off the
// channel — StopWatch is called and the state leaves StateWatching — even
// though the vanish-detect path (benefit absent) never triggers.
func TestWatcher_FreezeDetect_RotatesOnStalledMinutes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	backend := &freezeBackend{MockBackend: platformtest.New(), minutes: 5}

	notif := &recordingNotifier{}
	w := New(Config{
		AccountID:         "acc-freeze",
		Backend:           backend,
		Session:           platform.Session{AccessToken: "tok"},
		Notifier:          notif,
		TickInterval:      2 * time.Millisecond,
		HeartbeatInterval: 2 * time.Millisecond,
	})

	go func() { _ = w.Run(ctx) }()

	// Watcher reaches watching and observes the (flat) progress.
	require.Eventually(t, func() bool {
		return w.Snapshot().MinutesWatched >= 5
	}, time.Second, 2*time.Millisecond, "watcher never observed progress")

	// With minutes held flat, the freeze detector must rotate off the
	// channel within freezeThreshold polls.
	require.Eventually(t, func() bool {
		return backend.stops() >= 1
	}, 2*time.Second, 2*time.Millisecond, "watcher did not StopWatch on a frozen (non-advancing) benefit")

	// And it must not be sitting in StateWatching against the stalled
	// channel (the only channel is on cooldown, so it parks elsewhere).
	require.Eventually(t, func() bool {
		return w.State() != StateWatching
	}, time.Second, 2*time.Millisecond, "watcher stayed in StateWatching after freeze")
}

// TestWatcher_FreezeDetect_NoRotateWhenAdvancing: when MinutesWatched
// advances by 1 every poll the watch is healthy and must NOT rotate —
// guards against the freeze detector false-positiving on normal accrual.
func TestWatcher_FreezeDetect_NoRotateWhenAdvancing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	backend := &freezeBackend{MockBackend: platformtest.New(), advance: true}

	w := New(Config{
		AccountID:         "acc-advance",
		Backend:           backend,
		Session:           platform.Session{AccessToken: "tok"},
		Notifier:          &recordingNotifier{},
		TickInterval:      2 * time.Millisecond,
		HeartbeatInterval: 2 * time.Millisecond,
	})

	go func() { _ = w.Run(ctx) }()

	// Let many polls accrue (well past freezeThreshold) with minutes
	// advancing each time.
	require.Eventually(t, func() bool {
		return w.Snapshot().MinutesWatched >= freezeThreshold+5
	}, time.Second, 2*time.Millisecond, "watcher never accrued advancing progress")

	// A healthy, advancing watch must never have rotated.
	assert.Equal(t, 0, backend.stops(), "advancing watch must not trip the freeze detector")
	assert.Equal(t, StateWatching, w.State(), "advancing watch must stay in StateWatching")
}

func TestFirstUnmetPrecondition(t *testing.T) {
	claimed := map[string]bool{"dropA": true}
	// No preconditions -> always met.
	if got := firstUnmetPrecondition(nil, claimed); got != "" {
		t.Fatalf("empty preconditions should be met, got %q", got)
	}
	// All preconditions claimed -> met.
	if got := firstUnmetPrecondition([]string{"dropA"}, claimed); got != "" {
		t.Fatalf("claimed precondition should be met, got %q", got)
	}
	// Unmet precondition -> returns its id.
	if got := firstUnmetPrecondition([]string{"dropA", "dropB"}, claimed); got != "dropB" {
		t.Fatalf("want dropB unmet, got %q", got)
	}
}

// fieldRecordingNotifier captures both the event name and fields of every
// notification so a test can assert WHICH reward triggered a claim embed.
type fieldRecordingNotifier struct {
	mu     sync.Mutex
	events []string
	fields []map[string]any
}

func (r *fieldRecordingNotifier) Notify(_ context.Context, ev string, f map[string]any) error {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.fields = append(r.fields, f)
	r.mu.Unlock()
	return nil
}

// claimDrops returns the "drop" field of every recorded "claim" event.
func (r *fieldRecordingNotifier) claimDrops() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for i, ev := range r.events {
		if ev != "claim" {
			continue
		}
		if d, ok := r.fields[i]["drop"].(string); ok {
			out = append(out, d)
		}
	}
	return out
}

// A reward picked up by the multi-reward sweep (Kick CompletedSweeper) must
// fire the same "claim" Discord notification the benefit-complete path uses —
// otherwise swept sibling rewards are silent. The actively-mined
// currentBenefit must NOT be double-notified (the StateClaiming flow owns it),
// and a reward that resurfaces on a later poll must notify only once.
func TestWatcher_NotifySwept_SiblingNotifiesCurrentExcludedAndDeduped(t *testing.T) {
	ctx := context.Background()
	notif := &fieldRecordingNotifier{}
	w := New(Config{
		AccountID: "acc1",
		Backend:   platformtest.New(),
		Session:   platform.Session{AccessToken: "tok"},
		Notifier:  notif,
		Platform:  "kick",
	})
	// The currentBenefit is what the benefit-complete path claims + notifies.
	w.currentBenefit = &platform.DropBenefit{ID: "logo", Name: "Kick + Rust Wallpaper Logo"}

	sibling := platform.ClaimedReward{Game: "Rust", Title: "Kick + Rust Wallpaper Pattern"}
	current := platform.ClaimedReward{Game: "Rust", Title: "Kick + Rust Wallpaper Logo"}

	// Sweep returns both the sibling AND the currentBenefit (the dedupe case).
	w.notifySwept(ctx, sibling)
	w.notifySwept(ctx, current)
	// Next poll resurfaces the sibling (claim POST raced ahead of progress).
	w.notifySwept(ctx, sibling)

	drops := notif.claimDrops()
	require.Equal(t, []string{"Kick + Rust Wallpaper Pattern"}, drops,
		"only the sibling notifies: currentBenefit excluded, sibling deduped to one")
}

// nullGameBackend returns a single ACTIVE campaign with no game and one
// participating channel — the Kick "Football Drop" shape. ListEligibleChannels
// is inherited from MockBackend (returns a live stream), so a campaign that
// passes the filter will accrue heartbeats.
type nullGameBackend struct{ *platformtest.MockBackend }

func (n *nullGameBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	return []platform.Campaign{{
		ID: "c-football", Game: "", Status: "active", Name: "Football Drop",
		AllowedChannels: []string{"adrianozendejas32"},
		Benefits: []platform.DropBenefit{
			{ID: "drop1", CampaignID: "c-football", Name: "Jersey", RequiredMinutes: 2},
		},
	}}, nil
}

func TestWatcher_MinesNullGameWhenChannelWhitelisted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	backend := &nullGameBackend{platformtest.New()}
	w := New(Config{
		AccountID:    "acc1",
		Backend:      backend,
		Session:      platform.Session{AccessToken: "tok"},
		Notifier:     &recordingNotifier{},
		TickInterval: 5 * time.Millisecond,
		AllowGame:    func(g string) bool { return g == "Rust" }, // null game NOT whitelisted
		AllowChannel: func(chs []string) bool {
			for _, c := range chs {
				if c == "adrianozendejas32" {
					return true
				}
			}
			return false
		},
	})
	_ = w.Run(ctx)
	assert.Greater(t, backend.Heartbeats(), int64(0),
		"null-game campaign with a whitelisted channel must be mined")
}

func TestWatcher_SkipsNullGameWhenChannelNotWhitelisted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	backend := &nullGameBackend{platformtest.New()}
	w := New(Config{
		AccountID:    "acc1",
		Backend:      backend,
		Session:      platform.Session{AccessToken: "tok"},
		Notifier:     &recordingNotifier{},
		TickInterval: 5 * time.Millisecond,
		AllowGame:    func(g string) bool { return g == "Rust" },
		AllowChannel: func(chs []string) bool { return false },
	})
	_ = w.Run(ctx)
	assert.Equal(t, int64(0), backend.Heartbeats(),
		"null-game campaign with no matching channel must not be mined")
}

// idleBackend never returns campaigns, so the watcher goes idle — the
// trigger condition for force-watch (channel-points) to kick in.
type idleBackend struct{ *platformtest.MockBackend }

func (idleBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	return nil, nil
}

// fakeForce is a ForceWatchSource that always offers the same channel.
type fakeForce struct{ channel string }

func (f fakeForce) Next(_ context.Context, _ string) (ForceTask, bool) {
	if f.channel == "" {
		return ForceTask{}, false
	}
	return ForceTask{Channel: f.channel}, true
}

func TestWatcher_ForceWatchesWhenIdle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	backend := &idleBackend{platformtest.New()}
	w := New(Config{
		AccountID:         "acc1",
		Backend:           backend,
		Session:           platform.Session{AccessToken: "tok"},
		Notifier:          &recordingNotifier{},
		TickInterval:      5 * time.Millisecond,
		HeartbeatInterval: 5 * time.Millisecond,               // heartbeat every tick for test speed
		AllowGame:         func(string) bool { return false }, // nothing mineable
		ForceWatcher:      fakeForce{channel: "xqc"},
	})
	_ = w.Run(ctx)
	assert.Greater(t, backend.Heartbeats(), int64(0),
		"idle account with a force-watch channel must watch it for channel points")
}

func TestWatcher_NoForceWatchWhenDisabled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	backend := &idleBackend{platformtest.New()}
	w := New(Config{
		AccountID:    "acc1",
		Backend:      backend,
		Session:      platform.Session{AccessToken: "tok"},
		Notifier:     &recordingNotifier{},
		TickInterval: 5 * time.Millisecond,
		AllowGame:    func(string) bool { return false },
		ForceWatcher: fakeForce{channel: ""}, // disabled / no channel
	})
	_ = w.Run(ctx)
	assert.Equal(t, int64(0), backend.Heartbeats(),
		"idle account with force-watch disabled must not watch anything")
}

// multiTierBackend models a single Twitch campaign with four drops that
// grant the SAME reward at escalating watch-time thresholds (60/180/360/540m,
// issue #24). The first tier (t60) is claimed; its reward shows up as an
// OWNED gameEventDrops marker keyed by the shared RewardID. The remaining
// tiers stay in dropCampaignsInProgress (tracked, unclaimed). The watcher
// must keep mining the next unclaimed tier instead of treating the whole
// campaign as done because one tier's reward is owned.
type multiTierBackend struct {
	*platformtest.MockBackend
}

func (m *multiTierBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	mk := func(id string, mins int) platform.DropBenefit {
		return platform.DropBenefit{ID: id, CampaignID: "r6s", Name: "Esports Pack 2026 Stage 1", RequiredMinutes: mins, RewardID: "rewardX"}
	}
	return []platform.Campaign{{
		ID: "r6s", Game: "Rainbow Six Siege", Status: "active", AccountLinked: true,
		Benefits: []platform.DropBenefit{
			mk("t360", 360), mk("t540", 540), mk("t60", 60), mk("t180", 180),
		},
	}}, nil
}

func (m *multiTierBackend) InventoryProgress(_ context.Context, _ platform.Session) ([]platform.Progress, error) {
	return []platform.Progress{
		{BenefitID: "t60", MinutesWatched: 60, Claimed: true}, // tier 1 claimed in-progress row
		{BenefitID: "t180", MinutesWatched: 0, Claimed: false},
		{BenefitID: "t360", MinutesWatched: 0, Claimed: false},
		{BenefitID: "t540", MinutesWatched: 0, Claimed: false},
		{BenefitID: "rewardX", Claimed: true}, // gameEventDrops OWNED marker (shared reward)
	}, nil
}

func (m *multiTierBackend) ListEligibleChannels(_ context.Context, _ platform.Session, _ platform.Campaign) ([]platform.Stream, error) {
	return []platform.Stream{{Channel: "r6streamer", DropsEnabled: true}}, nil
}

// TestWatcher_MultiTierSameReward_ContinuesAfterFirstClaim is the #24
// regression: after the 60m tier is claimed, the watcher must pick the
// next unclaimed tier (the lowest remaining, 180m) and keep mining — not
// go idle because the shared reward is already owned.
func TestWatcher_MultiTierSameReward_ContinuesAfterFirstClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	backend := &multiTierBackend{MockBackend: platformtest.New()}
	w := New(Config{
		AccountID:    "acc_r6s",
		Backend:      backend,
		Session:      platform.Session{AccessToken: "tok"},
		Notifier:     &recordingNotifier{},
		TickInterval: 2 * time.Millisecond,
		AllowGame:    func(g string) bool { return g == "Rainbow Six Siege" },
	})

	go func() { _ = w.Run(ctx) }()

	require.Eventually(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.currentBenefit != nil && w.currentBenefit.ID == "t180"
	}, time.Second, 5*time.Millisecond,
		"watcher must mine the lowest unclaimed tier (t180) after the 60m tier's reward is owned, not go idle")
}

// recordingClaimRecorder captures every RecordClaimIfNew / PruneClaim call so
// a test can assert the reconcile loop's self-heal decision (issue #24).
type recordingClaimRecorder struct {
	mu         sync.Mutex
	recorded   []string
	pruned     []string
	claimedIDs map[string]bool
}

func (r *recordingClaimRecorder) ClaimedBenefitIDs(_ context.Context, _ string) (map[string]bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]bool{}
	for k, v := range r.claimedIDs {
		out[k] = v
	}
	return out, nil
}

func (r *recordingClaimRecorder) RecordClaim(_ context.Context, _ string, _ platform.DropBenefit) error {
	return nil
}

func (r *recordingClaimRecorder) RecordClaimIfNew(_ context.Context, _ string, b platform.DropBenefit) (bool, error) {
	r.mu.Lock()
	r.recorded = append(r.recorded, b.ID)
	r.mu.Unlock()
	return true, nil
}

func (r *recordingClaimRecorder) PruneClaim(_ context.Context, _ string, b platform.DropBenefit) (bool, error) {
	r.mu.Lock()
	r.pruned = append(r.pruned, b.ID)
	r.mu.Unlock()
	return true, nil
}

func (r *recordingClaimRecorder) snapshot() (recorded, pruned []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.recorded...), append([]string(nil), r.pruned...)
}

// TestWatcher_ReconcilePrunesStaleClaims is the #24 self-heal: when inventory
// tracks a drop as in-progress AND unclaimed, any pre-existing claims row for
// it is stale (the pre-v1.3.1 shared-reward bug wrote one per tier). The
// reconcile loop must PruneClaim those, and must NOT prune (or re-record) the
// tier that is genuinely claimed.
func TestWatcher_ReconcilePrunesStaleClaims(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	rec := &recordingClaimRecorder{}
	backend := &multiTierBackend{MockBackend: platformtest.New()}
	w := New(Config{
		AccountID:     "acc_r6s_prune",
		Backend:       backend,
		ClaimRecorder: rec,
		Session:       platform.Session{AccessToken: "tok"},
		Notifier:      &recordingNotifier{},
		TickInterval:  2 * time.Millisecond,
		AllowGame:     func(g string) bool { return g == "Rainbow Six Siege" },
	})

	go func() { _ = w.Run(ctx) }()

	require.Eventually(t, func() bool {
		_, pruned := rec.snapshot()
		return contains(pruned, "t180") && contains(pruned, "t360") && contains(pruned, "t540")
	}, time.Second, 5*time.Millisecond,
		"reconcile must prune stale claims for unclaimed in-progress tiers")

	recorded, pruned := rec.snapshot()
	assert.NotContains(t, pruned, "t60", "the genuinely claimed tier must not be pruned")
	assert.NotContains(t, recorded, "t180", "an unclaimed in-progress tier must not be re-recorded as claimed")
}

// TestWatcher_ForceCollected_ProtectsManualMark verifies the manual
// mark-collected override: a benefit covered by ForceCollected must NOT be
// pruned by the reconcile self-heal even while inventory reports it
// in-progress and unclaimed; the other unclaimed tiers must still be pruned.
func TestWatcher_ForceCollected_ProtectsManualMark(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	rec := &recordingClaimRecorder{}
	backend := &multiTierBackend{MockBackend: platformtest.New()}
	w := New(Config{
		AccountID:     "acc_r6s_protect",
		Backend:       backend,
		ClaimRecorder: rec,
		Session:       platform.Session{AccessToken: "tok"},
		Notifier:      &recordingNotifier{},
		TickInterval:  2 * time.Millisecond,
		AllowGame:     func(g string) bool { return g == "Rainbow Six Siege" },
		ForceCollected: func(_ string, benefitID string) bool {
			return benefitID == "t180" // user manually marked this tier collected
		},
	})

	go func() { _ = w.Run(ctx) }()

	// The unprotected unclaimed tiers must still be pruned.
	require.Eventually(t, func() bool {
		_, pruned := rec.snapshot()
		return contains(pruned, "t360") && contains(pruned, "t540")
	}, time.Second, 5*time.Millisecond, "unprotected stale tiers must still prune")

	_, pruned := rec.snapshot()
	assert.NotContains(t, pruned, "t180", "a ForceCollected benefit must never be pruned")
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// newCampaignSameRewardBackend models the #24 FOLLOW-UP: a brand-new
// campaign whose four tiers grant the SAME reward item (shared RewardID)
// the account already OWNS from a previous, now-ended campaign. The new
// tiers are NOT yet in the in-progress inventory (untracked — never
// watched), and InventoryProgress reports only the owned-reward marker.
// The watcher must IGNORE reward-ownership: the new campaign's drops are
// claimable again, so it must pick the lowest tier and must not mark any
// collected.
type newCampaignSameRewardBackend struct {
	*platformtest.MockBackend
}

func (b *newCampaignSameRewardBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	mk := func(id string, mins int) platform.DropBenefit {
		return platform.DropBenefit{ID: id, CampaignID: "r6s6", Name: "Esports Pack 2026 Stage 1", RequiredMinutes: mins, RewardID: "rewardX"}
	}
	return []platform.Campaign{{
		ID: "r6s6", Game: "Rainbow Six Siege", Status: "active", AccountLinked: true,
		Benefits: []platform.DropBenefit{mk("n360", 360), mk("n60", 60), mk("n180", 180), mk("n540", 540)},
	}}, nil
}

func (b *newCampaignSameRewardBackend) InventoryProgress(_ context.Context, _ platform.Session) ([]platform.Progress, error) {
	// Only the owned-reward marker (as the OLD gameEventDrops path would
	// have emitted). NONE of the new tiers are tracked or claimed per-drop.
	return []platform.Progress{{BenefitID: "rewardX", Claimed: true}}, nil
}

func (b *newCampaignSameRewardBackend) ListEligibleChannels(_ context.Context, _ platform.Session, _ platform.Campaign) ([]platform.Stream, error) {
	return []platform.Stream{{Channel: "rainbow6", DropsEnabled: true}}, nil
}

func TestWatcher_NewCampaignSameReward_NotSkippedOrFalseMarked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	rec := &recordingClaimRecorder{}
	backend := &newCampaignSameRewardBackend{MockBackend: platformtest.New()}
	w := New(Config{
		AccountID:     "acc_r6s6",
		Backend:       backend,
		ClaimRecorder: rec,
		Session:       platform.Session{AccessToken: "tok"},
		Notifier:      &recordingNotifier{},
		TickInterval:  2 * time.Millisecond,
		AllowGame:     func(g string) bool { return g == "Rainbow Six Siege" },
	})

	go func() { _ = w.Run(ctx) }()

	require.Eventually(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.currentBenefit != nil && w.currentBenefit.ID == "n60"
	}, time.Second, 5*time.Millisecond,
		"watcher must mine the new campaign's lowest tier even though it owns the same reward item from a prior campaign")

	recorded, _ := rec.snapshot()
	for _, id := range []string{"n60", "n180", "n360", "n540"} {
		assert.NotContains(t, recorded, id,
			"a new campaign's unclaimed tier must not be recorded collected on reward-ownership")
	}
}

// lowPriorityTrackedBackend models a campaign the bot is NOT actively mining
// (discovery returns it with NO populated benefits, as happens for a
// low-priority game like SMITE) but whose drop IS tracked in the in-progress
// inventory and unclaimed, with a leftover false claim row. The inventory
// sweep must prune it even though the campaign-benefits loop can't see it.
type lowPriorityTrackedBackend struct {
	*platformtest.MockBackend
}

func (b *lowPriorityTrackedBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	// Campaign present but benefits NOT populated (lazy/unmined) + a separate
	// mineable campaign so the watcher has something to do.
	return []platform.Campaign{
		{ID: "smite", Game: "SMITE 2", Status: "active", AccountLinked: true, Benefits: nil},
		{ID: "mine", Game: "Rust", Status: "active", AccountLinked: true,
			Benefits: []platform.DropBenefit{{ID: "rustdrop", CampaignID: "mine", RequiredMinutes: 9999}}},
	}, nil
}

func (b *lowPriorityTrackedBackend) InventoryProgress(_ context.Context, _ platform.Session) ([]platform.Progress, error) {
	// SMITE bundle tracked in-progress + UNCLAIMED (false claim row exists),
	// plus another claimed drop so the reconcile has claimed entries too.
	return []platform.Progress{
		{BenefitID: "smitebundle", MinutesWatched: 30, Claimed: false},
		{BenefitID: "somethingClaimed", MinutesWatched: 60, Claimed: true},
	}, nil
}

func (b *lowPriorityTrackedBackend) ListEligibleChannels(_ context.Context, _ platform.Session, _ platform.Campaign) ([]platform.Stream, error) {
	return []platform.Stream{{Channel: "ch", DropsEnabled: true}}, nil
}

// TestWatcher_InventorySweepPrunesUnminedTrackedFalseRow: a false claim row
// for a tracked-but-unclaimed drop in a campaign the bot isn't mining (no
// populated benefits) must still be pruned via the inventory sweep.
func TestWatcher_InventorySweepPrunesUnminedTrackedFalseRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	rec := &recordingClaimRecorder{}
	backend := &lowPriorityTrackedBackend{MockBackend: platformtest.New()}
	w := New(Config{
		AccountID:     "acc_smite",
		Backend:       backend,
		ClaimRecorder: rec,
		Session:       platform.Session{AccessToken: "tok"},
		Notifier:      &recordingNotifier{},
		TickInterval:  2 * time.Millisecond,
		AllowGame:     func(g string) bool { return g == "Rust" || g == "SMITE 2" },
	})

	go func() { _ = w.Run(ctx) }()

	require.Eventually(t, func() bool {
		_, pruned := rec.snapshot()
		return contains(pruned, "smitebundle")
	}, time.Second, 5*time.Millisecond,
		"inventory sweep must prune a tracked-unclaimed false row even when its campaign has no populated benefits")

	_, pruned := rec.snapshot()
	assert.NotContains(t, pruned, "somethingClaimed", "a claimed in-progress drop must never be pruned")
}

// alreadyClaimedBackend offers two unclaimed-in-inventory benefits; one of
// them already has a claim row (ownClaimed). The watcher must skip the
// already-claimed one and pick the other — without this it would re-mine a
// drop it already holds (the "watching an already-collected skin" waste).
type alreadyClaimedBackend struct {
	*platformtest.MockBackend
}

func (b *alreadyClaimedBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	return []platform.Campaign{{
		ID: "c1", Game: "Rust", Status: "active", AccountLinked: true,
		Benefits: []platform.DropBenefit{
			{ID: "done", CampaignID: "c1", Name: "Already Claimed", RequiredMinutes: 60},
			{ID: "todo", CampaignID: "c1", Name: "Still Open", RequiredMinutes: 60},
		},
	}}, nil
}
func (b *alreadyClaimedBackend) InventoryProgress(_ context.Context, _ platform.Session) ([]platform.Progress, error) {
	return nil, nil // neither appears in-progress (both departed/never-started)
}
func (b *alreadyClaimedBackend) ListEligibleChannels(_ context.Context, _ platform.Session, _ platform.Campaign) ([]platform.Stream, error) {
	return []platform.Stream{{Channel: "ch", DropsEnabled: true}}, nil
}

func TestWatcher_SkipsBenefitWeAlreadyClaimed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	rec := &recordingClaimRecorder{claimedIDs: map[string]bool{"done": true}}
	backend := &alreadyClaimedBackend{MockBackend: platformtest.New()}
	w := New(Config{
		AccountID:     "acc_x",
		Backend:       backend,
		ClaimRecorder: rec,
		Session:       platform.Session{AccessToken: "tok"},
		Notifier:      &recordingNotifier{},
		TickInterval:  2 * time.Millisecond,
		AllowGame:     func(g string) bool { return g == "Rust" },
	})
	go func() { _ = w.Run(ctx) }()

	require.Eventually(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.currentBenefit != nil && w.currentBenefit.ID == "todo"
	}, time.Second, 5*time.Millisecond,
		"watcher must skip the already-claimed benefit and pick the open one")

	w.mu.Lock()
	picked := ""
	if w.currentBenefit != nil {
		picked = w.currentBenefit.ID
	}
	w.mu.Unlock()
	assert.NotEqual(t, "done", picked, "must never pick a benefit we already hold a claim for")
}

// zeroMinBackend returns a single active campaign whose only drop is a
// 0-minute (sub/action) drop. The watcher must NOT pick it for mining.
type zeroMinBackend struct{ *platformtest.MockBackend }

func (b *zeroMinBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	return []platform.Campaign{{
		ID: "subcamp", Game: "League of Legends", Status: "active", AccountLinked: true,
		Benefits: []platform.DropBenefit{{ID: "sub1", CampaignID: "subcamp", Name: "Sub Drop", RequiredMinutes: 0}},
	}}, nil
}
func (b *zeroMinBackend) ListEligibleChannels(_ context.Context, _ platform.Session, _ platform.Campaign) ([]platform.Stream, error) {
	return []platform.Stream{{Channel: "lolesports", DropsEnabled: true}}, nil
}

// InventoryProgress returns nothing: 0-minute drops never appear in the watch
// inventory, so the mock must not return the default MockBackend drop1 row.
func (b *zeroMinBackend) InventoryProgress(_ context.Context, _ platform.Session) ([]platform.Progress, error) {
	return nil, nil
}

// TestWatcher_SkipsZeroMinuteDropForMining proves a 0-minute-only campaign is
// never picked: the watcher finds no eligible benefit and never starts watching.
func TestWatcher_SkipsZeroMinuteDropForMining(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	rec := &recordingClaimRecorder{}
	w := New(Config{
		AccountID:     "acc_zero",
		Backend:       &zeroMinBackend{MockBackend: platformtest.New()},
		ClaimRecorder: rec,
		Session:       platform.Session{AccessToken: "tok"},
		Notifier:      &recordingNotifier{},
		TickInterval:  2 * time.Millisecond,
		AllowGame:     func(g string) bool { return g == "League of Legends" },
	})
	go func() { _ = w.Run(ctx) }()
	<-ctx.Done()

	// Never claimed/recorded anything, and never settled into the watching state.
	recorded, pruned := rec.snapshot()
	if len(recorded) != 0 || len(pruned) != 0 {
		t.Fatalf("0-min campaign must not drive claims: recorded=%v pruned=%v", recorded, pruned)
	}
	if w.State() == StateWatching {
		t.Fatalf("watcher must not be watching a 0-minute-only campaign")
	}
}

// selfHealBackend serves one linked campaign whose benefit "healme" IS
// reported in the in-progress inventory (unclaimed). Used to prove a
// stale ghost-skip for that benefit gets cleared when it reappears.
type selfHealBackend struct {
	*platformtest.MockBackend
}

func (s *selfHealBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	return []platform.Campaign{{
		ID: "hcamp", Game: "HealGame", Name: "Heal Camp", Status: "active", AccountLinked: true,
		Benefits: []platform.DropBenefit{
			{ID: "healme", CampaignID: "hcamp", Name: "Heal Me", RequiredMinutes: 60},
		},
	}}, nil
}

func (s *selfHealBackend) InventoryProgress(_ context.Context, _ platform.Session) ([]platform.Progress, error) {
	// Twitch now reports the drop in-progress (enrolled, unclaimed) — the
	// signal that a prior ghost-skip was a false positive.
	return []platform.Progress{{BenefitID: "healme", MinutesWatched: 3, Claimed: false}}, nil
}

// TestWatcher_GhostSkip_SelfHealsWhenBenefitReappears verifies that a
// benefit pre-seeded into skippedBenefits (a persisted ghost-skip) is
// cleared — in-memory AND via SkipClearer — once it shows up in the
// in-progress inventory again, so a transient enrollment lag can't strand
// a legitimate drop permanently.
func TestWatcher_GhostSkip_SelfHealsWhenBenefitReappears(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	const accID = "acc-heal"
	var mu sync.Mutex
	cleared := map[string]bool{}

	backend := &selfHealBackend{MockBackend: platformtest.New()}
	w := New(Config{
		AccountID:         accID,
		Backend:           backend,
		Session:           platform.Session{AccessToken: "tok"},
		Notifier:          &recordingNotifier{},
		TickInterval:      2 * time.Millisecond,
		HeartbeatInterval: 2 * time.Millisecond,
		// Pre-seed the stale ghost-skip, as a fresh post-restart watcher would.
		PersistedSkips: func(string) (map[string]bool, error) {
			return map[string]bool{"healme": true}, nil
		},
		SkipClearer: func(_, benefitID string) error {
			mu.Lock()
			cleared[benefitID] = true
			mu.Unlock()
			return nil
		},
	})
	// Confirm it started skipped.
	w.mu.Lock()
	_, seeded := w.skippedBenefits["healme"]
	w.mu.Unlock()
	require.True(t, seeded, "healme should be pre-seeded as skipped")

	go func() { _ = w.Run(ctx) }()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return cleared["healme"]
	}, 2*time.Second, 5*time.Millisecond,
		"stale ghost-skip should be cleared once the benefit reappears in inventory")

	// And it must be gone from the in-memory set too.
	w.mu.Lock()
	_, stillSkipped := w.skippedBenefits["healme"]
	w.mu.Unlock()
	assert.False(t, stillSkipped, "healme must be removed from skippedBenefits after self-heal")
}

// ---- ordered-mode priority preemption tests ----

// preemptTestBackend is a controllable platform.Backend for preemption
// tests: the campaign set, channel map and failure modes can change
// mid-run while the watcher is live.
type preemptTestBackend struct {
	mu            sync.Mutex
	campaigns     []platform.Campaign
	campaignErr   error
	progress      map[string]int
	claimed       map[string]bool
	inventoryErr  error
	channels      map[string][]platform.Stream
	channelErr    error
	channelProbes map[string]int // ListEligibleChannels calls per campaign ID
	started       []string       // channels StartWatch was called on, in order
	stopped       []string       // channels StopWatch was called on, in order
	claimCalls    []string       // benefit IDs Claim was called with, in order
}

func (b *preemptTestBackend) Name() string { return "preempt-test" }
func (b *preemptTestBackend) StartDeviceLogin(_ context.Context) (platform.DeviceChallenge, error) {
	return platform.DeviceChallenge{}, nil
}
func (b *preemptTestBackend) PollDeviceLogin(_ context.Context, _ platform.DeviceChallenge) (platform.Session, error) {
	return platform.Session{}, nil
}
func (b *preemptTestBackend) LoginViaBrowser(_ context.Context, _ platform.BrowserRPC) (platform.Session, error) {
	return platform.Session{}, nil
}
func (b *preemptTestBackend) RefreshSession(_ context.Context, s platform.Session) (platform.Session, error) {
	return s, nil
}
func (b *preemptTestBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.campaignErr != nil {
		return nil, b.campaignErr
	}
	return append([]platform.Campaign(nil), b.campaigns...), nil
}
func (b *preemptTestBackend) ListEligibleChannels(_ context.Context, _ platform.Session, c platform.Campaign) ([]platform.Stream, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.channelProbes == nil {
		b.channelProbes = map[string]int{}
	}
	b.channelProbes[c.ID]++
	if b.channelErr != nil {
		return nil, b.channelErr
	}
	return append([]platform.Stream(nil), b.channels[c.ID]...), nil
}
func (b *preemptTestBackend) InventoryProgress(_ context.Context, _ platform.Session) ([]platform.Progress, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inventoryErr != nil {
		return nil, b.inventoryErr
	}
	out := make([]platform.Progress, 0, len(b.progress))
	for id, m := range b.progress {
		out = append(out, platform.Progress{BenefitID: id, MinutesWatched: m, Claimed: b.claimed[id]})
	}
	return out, nil
}
func (b *preemptTestBackend) StartWatch(_ context.Context, _ platform.Session, s platform.Stream) (platform.WatchHandle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.started = append(b.started, s.Channel)
	return platform.WatchHandle{Channel: s.Channel}, nil
}
func (b *preemptTestBackend) Heartbeat(_ context.Context, _ platform.WatchHandle) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id := range b.progress {
		b.progress[id]++
	}
	return nil
}
func (b *preemptTestBackend) StopWatch(_ context.Context, h platform.WatchHandle) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped = append(b.stopped, h.Channel)
	return nil
}
func (b *preemptTestBackend) Claim(_ context.Context, _ platform.Session, d platform.DropBenefit) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.claimCalls = append(b.claimCalls, d.ID)
	b.claimed[d.ID] = true
	return nil
}

func (b *preemptTestBackend) setCampaigns(cs []platform.Campaign) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.campaigns = cs
}
func (b *preemptTestBackend) setChannels(m map[string][]platform.Stream) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.channels = m
}
func (b *preemptTestBackend) setCampaignErr(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.campaignErr = err
}
func (b *preemptTestBackend) calls() (started, stopped, claims []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.started...), append([]string(nil), b.stopped...), append([]string(nil), b.claimCalls...)
}
func (b *preemptTestBackend) progressOf(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.progress[id]
}
func (b *preemptTestBackend) probeCount(ids ...string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := 0
	for _, id := range ids {
		total += b.channelProbes[id]
	}
	return total
}

func preemptCampaign(id, game, benefitID string, reqMin int) platform.Campaign {
	return platform.Campaign{
		ID: id, Game: game, Name: game + " campaign", Status: "active",
		Platform: "twitch", AccountLinked: true,
		Benefits: []platform.DropBenefit{
			{ID: benefitID, CampaignID: id, Name: benefitID + " drop", RequiredMinutes: reqMin},
		},
	}
}

func preemptRank(game string) int {
	switch game {
	case "GameA":
		return 0
	case "GameB":
		return 1
	case "GameC":
		return 2
	}
	return 1 << 30
}

// startPreemptWatcher builds a watcher with a shrunken preemption scan
// interval and runs it; the caller must cancel ctx AND wait on done
// before the test returns so the shrunken package var is restored only
// after the watcher goroutine has exited.
func startPreemptWatcher(t *testing.T, backend *preemptTestBackend, mode string) (*Watcher, context.Context, context.CancelFunc, chan struct{}) {
	t.Helper()
	old := preemptRecheckEvery
	preemptRecheckEvery = 30 * time.Millisecond
	t.Cleanup(func() { preemptRecheckEvery = old })
	w := New(Config{
		AccountID:    "acc_preempt",
		Backend:      backend,
		Session:      platform.Session{AccessToken: "tok"},
		Notifier:     &recordingNotifier{},
		TickInterval: 10 * time.Millisecond,
		AllowGame:    func(g string) bool { return true },
		GameRank:     preemptRank,
		PriorityMode: mode,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return w, ctx, cancel, done
}

func waitWatchingCampaign(t *testing.T, w *Watcher, campaignID string) {
	t.Helper()
	require.Eventually(t, func() bool {
		snap := w.Snapshot()
		return snap.State == StateWatching.String() && snap.CampaignID == campaignID
	}, 3*time.Second, 5*time.Millisecond, "watcher should be watching %s", campaignID)
}

// TestWatcher_PreemptHigherPriorityCampaign: watching a lower-priority
// game, a higher-priority game campaign with a live drops channel
// appears — the watcher must stop the current watch and switch without
// waiting for the current drop to complete.
func TestWatcher_PreemptHigherPriorityCampaign(t *testing.T) {
	campB := preemptCampaign("campB", "GameB", "dropB", 1000)
	backend := &preemptTestBackend{
		campaigns: []platform.Campaign{campB},
		progress:  map[string]int{"dropB": 0},
		claimed:   map[string]bool{},
		channels:  map[string][]platform.Stream{"campB": {{Channel: "streamerB", DropsEnabled: true}}},
	}
	w, _, _, _ := startPreemptWatcher(t, backend, "ordered")
	waitWatchingCampaign(t, w, "campB")

	campA := preemptCampaign("campA", "GameA", "dropA", 1000)
	backend.setCampaigns([]platform.Campaign{campB, campA})
	backend.setChannels(map[string][]platform.Stream{
		"campB": {{Channel: "streamerB", DropsEnabled: true}},
		"campA": {{Channel: "streamerA", DropsEnabled: true}},
	})
	backend.progress["dropA"] = 0

	waitWatchingCampaign(t, w, "campA")

	started, stopped, _ := backend.calls()
	assert.Contains(t, stopped, "streamerB", "old watch must be stopped on preemption")
	assert.Contains(t, started, "streamerA", "watcher must start watching the higher-priority campaign")
}

// TestWatcher_NoPreemptSameRank: a new campaign for the SAME game
// (same whitelist rank) must not preempt the current watch.
func TestWatcher_NoPreemptSameRank(t *testing.T) {
	campB := preemptCampaign("campB", "GameB", "dropB", 1000)
	backend := &preemptTestBackend{
		campaigns: []platform.Campaign{campB},
		progress:  map[string]int{"dropB": 0},
		claimed:   map[string]bool{},
		channels:  map[string][]platform.Stream{"campB": {{Channel: "streamerB", DropsEnabled: true}}},
	}
	w, _, _, _ := startPreemptWatcher(t, backend, "ordered")
	waitWatchingCampaign(t, w, "campB")

	campB2 := preemptCampaign("campB2", "GameB", "dropB2", 1000)
	backend.setCampaigns([]platform.Campaign{campB, campB2})
	backend.setChannels(map[string][]platform.Stream{
		"campB":  {{Channel: "streamerB", DropsEnabled: true}},
		"campB2": {{Channel: "streamerB2", DropsEnabled: true}},
	})
	backend.progress["dropB2"] = 0

	time.Sleep(300 * time.Millisecond) // several scan rounds
	snap := w.Snapshot()
	assert.Equal(t, "campB", snap.CampaignID, "same-rank campaign must not preempt")
	_, stopped, _ := backend.calls()
	assert.Empty(t, stopped, "no watch should be stopped")
}

// TestWatcher_NoPreemptLowerRank: a lower-priority game campaign must
// not preempt the current watch.
func TestWatcher_NoPreemptLowerRank(t *testing.T) {
	campB := preemptCampaign("campB", "GameB", "dropB", 1000)
	backend := &preemptTestBackend{
		campaigns: []platform.Campaign{campB},
		progress:  map[string]int{"dropB": 0},
		claimed:   map[string]bool{},
		channels:  map[string][]platform.Stream{"campB": {{Channel: "streamerB", DropsEnabled: true}}},
	}
	w, _, _, _ := startPreemptWatcher(t, backend, "ordered")
	waitWatchingCampaign(t, w, "campB")

	campC := preemptCampaign("campC", "GameC", "dropC", 1000)
	backend.setCampaigns([]platform.Campaign{campB, campC})
	backend.setChannels(map[string][]platform.Stream{
		"campB": {{Channel: "streamerB", DropsEnabled: true}},
		"campC": {{Channel: "streamerC", DropsEnabled: true}},
	})
	backend.progress["dropC"] = 0

	time.Sleep(300 * time.Millisecond)
	snap := w.Snapshot()
	assert.Equal(t, "campB", snap.CampaignID, "lower-rank campaign must not preempt")
	_, stopped, _ := backend.calls()
	assert.Empty(t, stopped, "no watch should be stopped")
}

// TestWatcher_NoPreemptWithoutCandidates: with no higher-priority
// candidate the watcher keeps its current target.
func TestWatcher_NoPreemptWithoutCandidates(t *testing.T) {
	campB := preemptCampaign("campB", "GameB", "dropB", 1000)
	backend := &preemptTestBackend{
		campaigns: []platform.Campaign{campB},
		progress:  map[string]int{"dropB": 0},
		claimed:   map[string]bool{},
		channels:  map[string][]platform.Stream{"campB": {{Channel: "streamerB", DropsEnabled: true}}},
	}
	w, _, _, _ := startPreemptWatcher(t, backend, "ordered")
	waitWatchingCampaign(t, w, "campB")

	time.Sleep(300 * time.Millisecond) // several scan rounds, nothing higher appears
	snap := w.Snapshot()
	assert.Equal(t, StateWatching.String(), snap.State)
	assert.Equal(t, "campB", snap.CampaignID)
	_, stopped, _ := backend.calls()
	assert.Empty(t, stopped, "no watch should be stopped")
}

// TestWatcher_NoPreemptWhenHigherHasNoLiveChannel: a higher-priority
// campaign whose channels are all offline must not tear down a healthy
// watch (pickCampaign would fall straight back to the current target).
func TestWatcher_NoPreemptWhenHigherHasNoLiveChannel(t *testing.T) {
	campB := preemptCampaign("campB", "GameB", "dropB", 1000)
	backend := &preemptTestBackend{
		campaigns: []platform.Campaign{campB},
		progress:  map[string]int{"dropB": 0},
		claimed:   map[string]bool{},
		channels:  map[string][]platform.Stream{"campB": {{Channel: "streamerB", DropsEnabled: true}}},
	}
	w, _, _, _ := startPreemptWatcher(t, backend, "ordered")
	waitWatchingCampaign(t, w, "campB")

	campA := preemptCampaign("campA", "GameA", "dropA", 1000)
	backend.setCampaigns([]platform.Campaign{campB, campA})
	// campA deliberately has no live channels.
	backend.setChannels(map[string][]platform.Stream{
		"campB": {{Channel: "streamerB", DropsEnabled: true}},
	})
	backend.progress["dropA"] = 0

	time.Sleep(300 * time.Millisecond)
	snap := w.Snapshot()
	assert.Equal(t, "campB", snap.CampaignID, "higher-rank campaign with no live channel must not preempt")
	_, stopped, _ := backend.calls()
	assert.Empty(t, stopped, "no watch should be stopped")
}

// TestWatcher_PreemptScanErrorBacksOff: a transient discovery failure
// must keep the current watch (never treated as "no candidates") and
// the scanner must recover once the backend is healthy again.
func TestWatcher_PreemptScanErrorBacksOff(t *testing.T) {
	campB := preemptCampaign("campB", "GameB", "dropB", 1000)
	backend := &preemptTestBackend{
		campaigns: []platform.Campaign{campB},
		progress:  map[string]int{"dropB": 0},
		claimed:   map[string]bool{},
		channels:  map[string][]platform.Stream{"campB": {{Channel: "streamerB", DropsEnabled: true}}},
	}
	w, _, _, _ := startPreemptWatcher(t, backend, "ordered")
	waitWatchingCampaign(t, w, "campB")

	backend.setCampaignErr(errors.New("transient gql boom"))
	time.Sleep(300 * time.Millisecond) // several scan rounds fail
	snap := w.Snapshot()
	assert.Equal(t, "campB", snap.CampaignID, "scan error must not interrupt the current watch")
	_, stopped, _ := backend.calls()
	assert.Empty(t, stopped, "no watch should be stopped on scan error")

	// Backend recovers and a higher-priority campaign appears: the
	// backoff must not have permanently disabled the scanner.
	backend.setCampaignErr(nil)
	campA := preemptCampaign("campA", "GameA", "dropA", 1000)
	backend.setCampaigns([]platform.Campaign{campB, campA})
	backend.setChannels(map[string][]platform.Stream{
		"campB": {{Channel: "streamerB", DropsEnabled: true}},
		"campA": {{Channel: "streamerA", DropsEnabled: true}},
	})
	backend.progress["dropA"] = 0
	waitWatchingCampaign(t, w, "campA")
}

// TestWatcher_PreemptPreservesProgress: preemption must not clear the
// already-credited server-side progress of the old drop and must not
// trigger a claim for it.
func TestWatcher_PreemptPreservesProgress(t *testing.T) {
	campB := preemptCampaign("campB", "GameB", "dropB", 1000)
	backend := &preemptTestBackend{
		campaigns: []platform.Campaign{campB},
		progress:  map[string]int{"dropB": 0},
		claimed:   map[string]bool{},
		channels:  map[string][]platform.Stream{"campB": {{Channel: "streamerB", DropsEnabled: true}}},
	}
	w, _, _, _ := startPreemptWatcher(t, backend, "ordered")
	waitWatchingCampaign(t, w, "campB")
	// Wait for the first heartbeat tick to credit server-side minutes.
	require.Eventually(t, func() bool { return backend.progressOf("dropB") > 0 },
		3*time.Second, 5*time.Millisecond, "some watch-time should have accrued before preemption")

	campA := preemptCampaign("campA", "GameA", "dropA", 1000)
	backend.setCampaigns([]platform.Campaign{campB, campA})
	backend.setChannels(map[string][]platform.Stream{
		"campB": {{Channel: "streamerB", DropsEnabled: true}},
		"campA": {{Channel: "streamerA", DropsEnabled: true}},
	})
	backend.progress["dropA"] = 0

	waitWatchingCampaign(t, w, "campA")

	assert.Greater(t, backend.progressOf("dropB"), 0,
		"preemption must not clear the old drop's credited progress")
	_, _, claims := backend.calls()
	assert.Empty(t, claims, "preemption must not trigger a claim for the abandoned drop")
}

// TestWatcher_NoPreemptInOtherModes: non-ordered pick modes
// (ending_soonest, low_avbl_first) never preempt, even when a
// higher-ranked campaign goes live.
func TestWatcher_NoPreemptInOtherModes(t *testing.T) {
	for _, mode := range []string{"ending_soonest", "low_avbl_first"} {
		t.Run(mode, func(t *testing.T) {
			campB := preemptCampaign("campB", "GameB", "dropB", 1000)
			backend := &preemptTestBackend{
				campaigns: []platform.Campaign{campB},
				progress:  map[string]int{"dropB": 0},
				claimed:   map[string]bool{},
				channels:  map[string][]platform.Stream{"campB": {{Channel: "streamerB", DropsEnabled: true}}},
			}
			w, _, _, _ := startPreemptWatcher(t, backend, mode)
			waitWatchingCampaign(t, w, "campB")

			campA := preemptCampaign("campA", "GameA", "dropA", 1000)
			backend.setCampaigns([]platform.Campaign{campB, campA})
			backend.setChannels(map[string][]platform.Stream{
				"campB": {{Channel: "streamerB", DropsEnabled: true}},
				"campA": {{Channel: "streamerA", DropsEnabled: true}},
			})
			backend.progress["dropA"] = 0

			time.Sleep(300 * time.Millisecond)
			snap := w.Snapshot()
			assert.Equal(t, "campB", snap.CampaignID, "mode %s must not preempt", mode)
			_, stopped, _ := backend.calls()
			assert.Empty(t, stopped, "no watch should be stopped in mode %s", mode)
		})
	}
}

// The top-ranked candidate has no live channel but the next-ranked one
// does: the scan must walk down the ranking and preempt to the first
// actually-available higher-priority target (campB), not give up.
func TestWatcher_PreemptWalksDownToNextAvailable(t *testing.T) {
	campC := preemptCampaign("campC", "GameC", "dropC", 1000)
	backend := &preemptTestBackend{
		campaigns: []platform.Campaign{campC},
		progress:  map[string]int{"dropC": 0},
		claimed:   map[string]bool{},
		channels:  map[string][]platform.Stream{"campC": {{Channel: "streamerC", DropsEnabled: true}}},
	}
	w, _, _, _ := startPreemptWatcher(t, backend, "ordered")
	waitWatchingCampaign(t, w, "campC")

	// campA is the highest-ranked (GameA) but has NO live channels;
	// campB (GameB) is live and should win the preemption.
	campA := preemptCampaign("campA", "GameA", "dropA", 1000)
	campB := preemptCampaign("campB", "GameB", "dropB", 1000)
	backend.setCampaigns([]platform.Campaign{campC, campA, campB})
	backend.setChannels(map[string][]platform.Stream{
		"campC": {{Channel: "streamerC", DropsEnabled: true}},
		"campB": {{Channel: "streamerB", DropsEnabled: true}},
	})

	waitWatchingCampaign(t, w, "campB")
	started, stopped, _ := backend.calls()
	assert.Contains(t, stopped, "streamerC", "current watch must be stopped before switching")
	assert.Contains(t, started, "streamerB", "must start watching the live next-ranked candidate")
}

// A long tail of offline higher-ranked campaigns must not turn every
// scan into a GQL fan-out: channel probes per scan are capped.
func TestWatcher_PreemptChannelProbeCap(t *testing.T) {
	campF := preemptCampaign("campF", "GameC", "dropF", 1000)
	campaigns := []platform.Campaign{campF}
	offIDs := []string{"campH0", "campH1", "campH2", "campH3", "campH4"}
	for i, id := range offIDs {
		campaigns = append(campaigns, preemptCampaign(id, "GameA", "dropH"+string(rune('0'+i)), 1000))
	}
	backend := &preemptTestBackend{
		campaigns: campaigns,
		progress:  map[string]int{"dropF": 0},
		claimed:   map[string]bool{},
		channels:  map[string][]platform.Stream{"campF": {{Channel: "streamerF", DropsEnabled: true}}},
	}
	w := New(Config{
		AccountID:    "acc-preempt-cap",
		Session:      platform.Session{},
		Backend:      backend,
		GameRank:     preemptRank,
		PriorityMode: "ordered",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Seed the watch state the way pickStream leaves it.
	w.mu.Lock()
	w.currentCampaign = &campF
	h := platform.WatchHandle{Channel: "streamerF"}
	w.handle = &h
	w.mu.Unlock()

	preempted := w.maybePreempt(ctx)
	assert.False(t, preempted, "no live higher-ranked channel: must not preempt")
	assert.LessOrEqual(t, backend.probeCount(offIDs...), maxPreemptChannelProbes,
		"channel probes per preemption scan must be capped")
}

// Starvation regression: the first three higher-ranked candidates are
// persistently offline and the fourth is live. With the per-scan probe
// cap, a scan that always restarts at the top would never reach the
// fourth candidate. Round-robin resume must preempt to it within a
// bounded number of scans.
func TestWatcher_PreemptRoundRobinNoStarvation(t *testing.T) {
	campF := preemptCampaign("campF", "GameC", "dropF", 1000)
	backend := &preemptTestBackend{
		campaigns: []platform.Campaign{campF},
		progress:  map[string]int{"dropF": 0},
		claimed:   map[string]bool{},
		channels:  map[string][]platform.Stream{"campF": {{Channel: "streamerF", DropsEnabled: true}}},
	}
	w, _, _, _ := startPreemptWatcher(t, backend, "ordered")
	waitWatchingCampaign(t, w, "campF")

	h0 := preemptCampaign("campH0", "GameA", "dropH0", 1000)
	h1 := preemptCampaign("campH1", "GameA", "dropH1", 1000)
	h2 := preemptCampaign("campH2", "GameA", "dropH2", 1000)
	h3 := preemptCampaign("campH3", "GameA", "dropH3", 1000)
	backend.setCampaigns([]platform.Campaign{campF, h0, h1, h2, h3})
	backend.setChannels(map[string][]platform.Stream{
		"campF":  {{Channel: "streamerF", DropsEnabled: true}},
		"campH3": {{Channel: "streamerH3", DropsEnabled: true}},
	})

	// Scan 1 probes campH0..campH2 (offline, cursor advances); scan 2
	// resumes at campH3 (live) and preempts. The pick flow then skips
	// the channelless H0..H2 and lands on campH3. The 3s waiter is the
	// bounded-time assertion: without round-robin resume, campH3 would
	// never be reached.
	waitWatchingCampaign(t, w, "campH3")
	started, stopped, _ := backend.calls()
	assert.Contains(t, stopped, "streamerF", "current watch must be stopped before switching")
	assert.Contains(t, started, "streamerH3", "must reach the live fourth candidate, not starve below the cap")
}

// Deterministic cursor check: with five offline higher-ranked
// candidates and a cap of three, consecutive scans must probe
// disjoint slices and wrap around, never re-scanning only the top.
func TestWatcher_PreemptCursorCycles(t *testing.T) {
	campF := preemptCampaign("campF", "GameC", "dropF", 1000)
	ids := []string{"campH0", "campH1", "campH2", "campH3", "campH4"}
	campaigns := []platform.Campaign{campF}
	for i, id := range ids {
		campaigns = append(campaigns, preemptCampaign(id, "GameA", "dropH"+string(rune('0'+i)), 1000))
	}
	backend := &preemptTestBackend{
		campaigns: campaigns,
		progress:  map[string]int{"dropF": 0},
		claimed:   map[string]bool{},
		channels:  map[string][]platform.Stream{"campF": {{Channel: "streamerF", DropsEnabled: true}}},
	}
	w := New(Config{
		AccountID:    "acc-preempt-cycle",
		Session:      platform.Session{},
		Backend:      backend,
		GameRank:     preemptRank,
		PriorityMode: "ordered",
	})
	w.mu.Lock()
	w.currentCampaign = &campF
	h := platform.WatchHandle{Channel: "streamerF"}
	w.handle = &h
	w.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	assert.False(t, w.maybePreempt(ctx))
	assert.Equal(t, 1, backend.probeCount("campH0"))
	assert.Equal(t, 1, backend.probeCount("campH1"))
	assert.Equal(t, 1, backend.probeCount("campH2"))
	assert.Equal(t, 0, backend.probeCount("campH3", "campH4"), "scan 1 must stop at the cap")

	assert.False(t, w.maybePreempt(ctx))
	assert.Equal(t, 1, backend.probeCount("campH3"), "scan 2 must resume after the cursor")
	assert.Equal(t, 1, backend.probeCount("campH4"))

	assert.False(t, w.maybePreempt(ctx))
	assert.Equal(t, 2, backend.probeCount("campH0"), "scan 3 must wrap to the top after a full cycle")
}
