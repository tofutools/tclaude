package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TUIColorScheme defaults to "default" when unconfigured, returns a known
// scheme verbatim, and falls back to the default for a blank / hand-edited
// garbage value — so a typo can never leave the watch views unstyled.
func TestTUIColorScheme(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want string
	}{
		{"nil config", nil, TUIColorSchemeDefault},
		{"absent block", &Config{}, TUIColorSchemeDefault},
		{"empty scheme", &Config{TUI: &TUIConfig{ColorScheme: ""}}, TUIColorSchemeDefault},
		{"explicit default", &Config{TUI: &TUIConfig{ColorScheme: TUIColorSchemeDefault}}, TUIColorSchemeDefault},
		{"high contrast", &Config{TUI: &TUIConfig{ColorScheme: TUIColorSchemeHighContrast}}, TUIColorSchemeHighContrast},
		{"unknown falls back", &Config{TUI: &TUIConfig{ColorScheme: "neon"}}, TUIColorSchemeDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.cfg.TUIColorScheme())
		})
	}
}

// The scheme constants are the exact on-disk / dashboard strings; pin them so
// a rename here can't silently drift from the Config tab or tuistyle.
func TestTUIColorSchemeConstants(t *testing.T) {
	assert.Equal(t, "default", TUIColorSchemeDefault)
	assert.Equal(t, "dark-high-contrast", TUIColorSchemeHighContrast)
}

// Validate rejects a non-empty unknown scheme (so the Config tab tells the
// human) but accepts the known schemes, an empty scheme, and an absent block.
func TestValidate_TUIColorScheme(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TUI = &TUIConfig{ColorScheme: "neon"}
	assert.Contains(t, strings.Join(Validate(cfg), " | "), "tui.color_scheme")

	accepts := func(scheme string) {
		c := DefaultConfig()
		c.TUI = &TUIConfig{ColorScheme: scheme}
		for _, e := range Validate(c) {
			assert.NotContains(t, e, "tui.color_scheme", "scheme %q should be accepted", scheme)
		}
	}
	accepts("")
	accepts(TUIColorSchemeDefault)
	accepts(TUIColorSchemeHighContrast)

	// An absent block is never an error.
	for _, e := range Validate(DefaultConfig()) {
		assert.NotContains(t, e, "tui.color_scheme")
	}
}

// The block round-trips through JSON with omitempty: an absent block marshals
// to nothing (so the dashboard diff stays clean), and a set value survives a
// marshal→unmarshal cycle. This is the contract the Config tab editor relies on.
func TestTUI_JSONRoundTrip(t *testing.T) {
	// Absent block → no key.
	raw, err := json.Marshal(&Config{})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "tui")

	// Set value survives the round-trip.
	in := &Config{TUI: &TUIConfig{ColorScheme: TUIColorSchemeHighContrast}}
	raw, err = json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "color_scheme")

	var out Config
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotNil(t, out.TUI)
	assert.Equal(t, TUIColorSchemeHighContrast, out.TUI.ColorScheme)
}
