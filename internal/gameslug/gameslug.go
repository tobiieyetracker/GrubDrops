// Package gameslug normalizes game names into the canonical slug + id forms
// used across the app (Twitch directory lookups, the games table, campaign
// persistence). Previously each package carried its own copy (twitch
// slugify, api slugifyGame, store slugFromName/gameIDFromName); they could
// drift on edge cases. This is the single source of truth.
package gameslug

import "strings"

// Slug normalizes a game name to its canonical dash slug:
//
//	"Apex Legends"      -> "apex-legends"
//	"Counter-Strike 2"  -> "counter-strike-2"
//	"Tom's Game!!"      -> "toms-game"
//
// Lowercases; runs of space / '-' / '_' collapse to a single dash; every
// other character (apostrophes, periods, colons, ...) is dropped; leading
// and trailing dashes are trimmed.
func Slug(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			out = append(out, c)
		case c == ' ' || c == '-' || c == '_':
			if len(out) > 0 && out[len(out)-1] != '-' {
				out = append(out, '-')
			}
		}
		// everything else is dropped
	}
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	return string(out)
}

// ID returns the internal game id: "g_" + the slug with dashes as
// underscores ("Apex Legends" -> "g_apex_legends"). The g_ prefix keeps
// generated ids from colliding with user-entered ones.
func ID(name string) string {
	return "g_" + strings.ReplaceAll(Slug(name), "-", "_")
}

// twitchSlugFixups corrects the naive Slug() guess for games whose real
// Twitch directory slug differs. Slug() is a local guess from the display
// name; Twitch's canonical slug for these games is different, and the
// directory query returns game=null for the guessed slug — silently
// dropping every drop campaign of the game from discovery. Keyed by the
// naive slug so both the display name and an already-slugified input hit
// the same fixup. Verified 2026-10-01 via DirectoryPage_Game /
// DirectoryGameRedirect against gql.twitch.tv:
//   - "Rainbow Six Siege" -> tom-clancys-rainbow-six-siege (was: rainbow-six-siege)
//   - "PUBG: Black Budget" -> project-bb (was: pubg-black-budget)
//
// Long-term this should be replaced by resolving slugs through the
// DirectoryGameRedirect operation at runtime.
var twitchSlugFixups = map[string]string{
	"rainbow-six-siege": "tom-clancys-rainbow-six-siege",
	"pubg-black-budget": "project-bb",
}

// TwitchSlug returns the slug to use for Twitch directory lookups
// (DirectoryPage_Game). It is Slug() with the known-wrong guesses
// corrected via twitchSlugFixups. ID() intentionally still derives from
// the display name so existing game ids (g_rainbow_six_siege, …) are
// stable; only the directory-lookup slug changes.
func TwitchSlug(name string) string {
	s := Slug(name)
	if fix, ok := twitchSlugFixups[s]; ok {
		return fix
	}
	return s
}
