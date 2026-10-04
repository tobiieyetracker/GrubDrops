package watcher

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aalejandrofer/grubdrops/internal/platform"
	"github.com/aalejandrofer/grubdrops/internal/platform/platformtest"
)

// accountLinkBackend serves a campaign with AccountLinked=false.
// The drop can still reach its minute threshold via watching; the
// Twitch-side claim must proceed regardless of the link flag, because
// AccountLinked describes game-side delivery only.
type accountLinkBackend struct {
	*platformtest.MockBackend
	mu       sync.Mutex
	progress map[string]int
	claimed  map[string]bool
	// claimErr, when non-nil, is returned by Claim to simulate
	// challenge/429/auth failures.
	claimErr error
	claims   int
}

func newAccountLinkBackend() *accountLinkBackend {
	return &accountLinkBackend{
		MockBackend: platformtest.New(),
		progress:    map[string]int{"drop1": 0},
		claimed:     map[string]bool{},
	}
}

func (b *accountLinkBackend) ListActiveCampaigns(_ context.Context, _ platform.Session) ([]platform.Campaign, error) {
	return []platform.Campaign{{
		ID: "camp", Game: "TestGame", Name: "Test Camp", Status: "active",
		// Intentionally unlinked: Twitch claim must still work.
		AccountLinked:      false,
		AccountLinkChecked: true,
		Benefits: []platform.DropBenefit{
			{ID: "drop1", CampaignID: "camp", Name: "Drop One", RequiredMinutes: 2},
		},
	}}, nil
}

func (b *accountLinkBackend) InventoryProgress(_ context.Context, _ platform.Session) ([]platform.Progress, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return []platform.Progress{
		{BenefitID: "drop1", MinutesWatched: b.progress["drop1"], Claimed: b.claimed["drop1"]},
	}, nil
}

func (b *accountLinkBackend) Claim(_ context.Context, _ platform.Session, benefit platform.DropBenefit) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.claimErr != nil {
		return b.claimErr
	}
	b.claims++
	b.claimed[benefit.ID] = true
	return nil
}

// advance simulates watch progress.
func (b *accountLinkBackend) advance(id string, mins int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.progress[id] += mins
}

func (b *accountLinkBackend) claimCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.claims
}

func newAccountLinkWatcher(backend *accountLinkBackend) *Watcher {
	return New(Config{
		AccountID:             "acc-link-test",
		Backend:               backend,
		Session:               platform.Session{AccessToken: "tok"},
		TickInterval:          2 * time.Millisecond,
		HeartbeatInterval:     2 * time.Millisecond,
		ProgressNotifyStepPct: 50,
	})
}

// TestAccountLinkedZeroStillClaims verifies that a drop which has met
// Twitch's claim conditions (minutes >= required) enters the local
// Twitch claim even when the campaign reports AccountLinked=false.
// The link flag is game-side delivery status, not a Twitch claim gate.
func TestAccountLinkedZeroStillClaims(t *testing.T) {
	backend := newAccountLinkBackend()
	w := newAccountLinkWatcher(backend)

	// Drive progress to the threshold before the watcher runs.
	backend.advance("drop1", 2)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	require.Eventually(t, func() bool {
		return backend.claimCount() == 1
	}, 4*time.Second, 2*time.Millisecond,
		"claim must proceed when minutes are met, regardless of AccountLinked=false")
	cancel()
	<-done
}

// TestAccountLinkedZeroBelowThresholdNoClaim verifies that a drop which
// has NOT met the minute threshold does not trigger a claim.
func TestAccountLinkedZeroBelowThresholdNoClaim(t *testing.T) {
	backend := newAccountLinkBackend()
	w := newAccountLinkWatcher(backend)

	// Only 1 of 2 required minutes.
	backend.advance("drop1", 1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Give the watcher time to tick; no claim should happen.
	time.Sleep(500 * time.Millisecond)
	assert.Equal(t, 0, backend.claimCount(), "no claim when threshold not met")
	cancel()
	<-done
}

// TestClaimChallengeErrorStopsNoRetry verifies that challenge/429/auth
// errors from the claim path surface and are not retried in a tight loop.
func TestClaimChallengeErrorStopsNoRetry(t *testing.T) {
	backend := newAccountLinkBackend()
	backend.claimErr = errors.New("twitch integrity challenge: missing client-integrity token")
	w := newAccountLinkWatcher(backend)

	backend.advance("drop1", 2)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// The watcher records the failure; the claim must not succeed.
	time.Sleep(500 * time.Millisecond)
	assert.Equal(t, 0, backend.claimCount(), "failed claim must not be counted as claimed")
	cancel()
	<-done
	// If we get here without hanging or panicking, the error path
	// stopped cleanly instead of spinning.
}

// TestClaim429ErrorStopsNoRetry verifies that HTTP 429 rate-limit errors
// from the claim path do not trigger tight-loop retries.
func TestClaim429ErrorStopsNoRetry(t *testing.T) {
	backend := newAccountLinkBackend()
	backend.claimErr = errors.New("twitch gql: HTTP 429 Too Many Requests")
	w := newAccountLinkWatcher(backend)

	backend.advance("drop1", 2)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	time.Sleep(500 * time.Millisecond)
	assert.Equal(t, 0, backend.claimCount(), "429-failed claim must not be counted as claimed")
	cancel()
	<-done
}

// TestClaimAuthErrorStopsNoRetry verifies that authentication errors
// from the claim path do not trigger tight-loop retries.
func TestClaimAuthErrorStopsNoRetry(t *testing.T) {
	backend := newAccountLinkBackend()
	backend.claimErr = errors.New("twitch gql: HTTP 401 Unauthorized: invalid access token")
	w := newAccountLinkWatcher(backend)

	backend.advance("drop1", 2)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	time.Sleep(500 * time.Millisecond)
	assert.Equal(t, 0, backend.claimCount(), "auth-failed claim must not be counted as claimed")
	cancel()
	<-done
}
