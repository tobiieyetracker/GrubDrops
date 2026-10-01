package gameslug

import "testing"

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"":                     "",
		"Apex Legends":         "apex-legends",
		"Counter-Strike 2":     "counter-strike-2",
		"World of Warcraft":    "world-of-warcraft",
		"Dead by Daylight":     "dead-by-daylight",
		"Dota 2":               "dota-2",
		"Tom's Game!!":         "toms-game",
		"  spaced  out  ":      "spaced-out",
		"under_score_name":     "under-score-name",
		"--leading-trailing--": "leading-trailing",
		"!!!":                  "",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestID(t *testing.T) {
	cases := map[string]string{
		"Apex Legends":     "g_apex_legends",
		"Counter-Strike 2": "g_counter_strike_2",
		"":                 "g_",
	}
	for in, want := range cases {
		if got := ID(in); got != want {
			t.Errorf("ID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTwitchSlug(t *testing.T) {
	cases := map[string]string{
		// Known-wrong naive guesses corrected to the real directory slug.
		"Rainbow Six Siege":  "tom-clancys-rainbow-six-siege",
		"rainbow-six-siege":  "tom-clancys-rainbow-six-siege",
		"PUBG: Black Budget": "project-bb",
		"pubg-black-budget":  "project-bb",
		// Everything else passes through the naive Slug().
		"Apex Legends": "apex-legends",
		"Rust":         "rust",
		"":             "",
	}
	for in, want := range cases {
		if got := TwitchSlug(in); got != want {
			t.Errorf("TwitchSlug(%q) = %q, want %q", in, got, want)
		}
	}
	// ID stays derived from the display name so existing game ids are stable.
	if got := ID("Rainbow Six Siege"); got != "g_rainbow_six_siege" {
		t.Errorf("ID(%q) = %q, want %q", "Rainbow Six Siege", got, "g_rainbow_six_siege")
	}
}
