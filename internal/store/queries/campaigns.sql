-- name: UpsertCampaign :exec
INSERT INTO campaigns (id, platform, game, name, starts_at, ends_at, status, raw_json, discovered_at, kind, account_linked, account_link_url, twitch_game_id, twitch_game_slug, starts_at_source, ends_at_source)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    name = excluded.name,
    starts_at = CASE
        WHEN excluded.starts_at_source IN ('twitch', 'kick') THEN excluded.starts_at
        WHEN campaigns.starts_at_source IN ('twitch', 'kick') THEN campaigns.starts_at
        ELSE 0
    END,
    ends_at = CASE
        WHEN excluded.ends_at_source IN ('twitch', 'kick') THEN excluded.ends_at
        WHEN campaigns.ends_at_source IN ('twitch', 'kick') THEN campaigns.ends_at
        ELSE 0
    END,
    starts_at_source = CASE
        WHEN excluded.starts_at_source IN ('twitch', 'kick') THEN excluded.starts_at_source
        WHEN campaigns.starts_at_source IN ('twitch', 'kick') THEN campaigns.starts_at_source
        ELSE 'unknown'
    END,
    ends_at_source = CASE
        WHEN excluded.ends_at_source IN ('twitch', 'kick') THEN excluded.ends_at_source
        WHEN campaigns.ends_at_source IN ('twitch', 'kick') THEN campaigns.ends_at_source
        ELSE 'unknown'
    END,
    twitch_game_id = CASE WHEN excluded.twitch_game_id <> '' THEN excluded.twitch_game_id ELSE campaigns.twitch_game_id END,
    twitch_game_slug = CASE WHEN excluded.twitch_game_slug <> '' THEN excluded.twitch_game_slug ELSE campaigns.twitch_game_slug END,
    status = excluded.status,
    raw_json = excluded.raw_json,
    kind = excluded.kind,
    account_linked = excluded.account_linked,
    account_link_url = excluded.account_link_url;

-- name: UpsertBenefit :exec
INSERT INTO benefits (id, campaign_id, name, required_minutes, image_url)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    name = excluded.name,
    required_minutes = excluded.required_minutes,
    image_url = excluded.image_url;

-- name: ListActiveCampaignsForPlatform :many
SELECT id, platform, game, name,
    CASE WHEN starts_at_source IN ('twitch', 'kick') THEN starts_at ELSE 0 END AS starts_at,
    CASE WHEN ends_at_source IN ('twitch', 'kick') THEN ends_at ELSE 0 END AS ends_at,
    status, raw_json, discovered_at, kind, account_linked, account_link_url
FROM campaigns
WHERE platform = ? AND status = 'active'
  AND (starts_at_source NOT IN ('twitch', 'kick') OR starts_at <= ?)
  AND (ends_at_source NOT IN ('twitch', 'kick') OR ends_at >= ?)
ORDER BY discovered_at DESC;

-- name: ListBenefitsForCampaign :many
SELECT * FROM benefits WHERE campaign_id = ?;

-- name: ListClaimsForCampaign :many
-- Which accounts have claimed each benefit in a campaign. Powers the
-- per-account COLLECTED marks on the /drops expanded item list.
SELECT c.benefit_id, a.id AS account_id, a.platform, a.display_name
FROM claims c
JOIN accounts a ON a.id = c.account_id
JOIN benefits b ON b.id = c.benefit_id
WHERE b.campaign_id = ?;

-- name: GetCampaign :one
SELECT id, platform, game, name,
    CASE WHEN starts_at_source IN ('twitch', 'kick') THEN starts_at ELSE 0 END AS starts_at,
    CASE WHEN ends_at_source IN ('twitch', 'kick') THEN ends_at ELSE 0 END AS ends_at,
    status, raw_json, discovered_at, kind, account_linked, account_link_url
FROM campaigns WHERE id = ?;

-- name: ListPastCampaigns :many
-- Campaigns that have ended. Whitelist filtering is applied in Go.
SELECT id, platform, game, name,
    CASE WHEN starts_at_source IN ('twitch', 'kick') THEN starts_at ELSE 0 END AS starts_at,
    CASE WHEN ends_at_source IN ('twitch', 'kick') THEN ends_at ELSE 0 END AS ends_at,
    status, raw_json, discovered_at, kind, account_linked, account_link_url
FROM campaigns
WHERE status = 'expired' OR (ends_at_source IN ('twitch', 'kick') AND ends_at < ?)
ORDER BY CASE WHEN ends_at_source IN ('twitch', 'kick') THEN ends_at ELSE discovered_at END DESC
LIMIT ?;

-- name: ListCurrentCampaigns :many
-- Campaigns currently in flight with provider-confirmed time bounds when available.
-- Whitelist filtering is applied in Go.
SELECT id, platform, game, name,
    CASE WHEN starts_at_source IN ('twitch', 'kick') THEN starts_at ELSE 0 END AS starts_at,
    CASE WHEN ends_at_source IN ('twitch', 'kick') THEN ends_at ELSE 0 END AS ends_at,
    status, raw_json, discovered_at, kind, account_linked, account_link_url
FROM campaigns
WHERE status = 'active'
  AND (starts_at_source NOT IN ('twitch', 'kick') OR starts_at <= ?)
  AND (ends_at_source NOT IN ('twitch', 'kick') OR ends_at > ?)
ORDER BY CASE WHEN ends_at_source IN ('twitch', 'kick') THEN 0 ELSE 1 END, ends_at ASC
LIMIT ?;

-- name: ListUpcomingCampaigns :many
-- Campaigns announced but not yet started. Whitelist filtering is
-- applied in Go.
SELECT id, platform, game, name,
    CASE WHEN starts_at_source IN ('twitch', 'kick') THEN starts_at ELSE 0 END AS starts_at,
    CASE WHEN ends_at_source IN ('twitch', 'kick') THEN ends_at ELSE 0 END AS ends_at,
    status, raw_json, discovered_at, kind, account_linked, account_link_url
FROM campaigns
WHERE status = 'upcoming' OR (starts_at_source IN ('twitch', 'kick') AND starts_at > ?)
ORDER BY CASE WHEN starts_at_source IN ('twitch', 'kick') THEN 0 ELSE 1 END, starts_at ASC
LIMIT ?;

-- name: UpsertAccountCampaignLink :exec
INSERT INTO account_campaign_links (account_id, campaign_id, linked, checked, link_url, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(account_id, campaign_id) DO UPDATE SET
    linked = excluded.linked,
    checked = excluded.checked,
    link_url = excluded.link_url,
    updated_at = excluded.updated_at;

-- name: ListAccountLinksForCampaign :many
-- Per-account link state for a campaign, joined with the account handle.
-- Drives the per-account connect chips on the not-linked table.
SELECT a.id AS account_id, a.platform, a.display_name, l.linked, l.checked, l.link_url
FROM account_campaign_links l
JOIN accounts a ON a.id = l.account_id
WHERE l.campaign_id = ?
ORDER BY a.display_name ASC;
