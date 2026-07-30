package common

import (
	"strings"
	"testing"
)

// TmuxClientFeatureArgs feeds tmux's -T flag from the process environment. tmux
// rejects an unparsable feature list by failing the whole client, so a bad value
// would cost a blank terminal rather than a terminal without hyperlinks — hence
// the charset gate, and hence pinning it.
func TestTmuxClientFeatureArgs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  []string
	}{
		{"hyperlinks", TmuxHyperlinksFeature, []string{"-T", "hyperlinks"}},
		{"comma list", "hyperlinks,sync", []string{"-T", "hyperlinks,sync"}},
		{"hyphen and digits", "256,osc7,rgb-x", []string{"-T", "256,osc7,rgb-x"}},
		{"surrounding space trimmed", "  hyperlinks  ", []string{"-T", "hyperlinks"}},
		{"empty is absent", "", nil},
		{"whitespace only is absent", "   ", nil},
		{"inner space rejected", "hyper links", nil},
		{"uppercase rejected", "Hyperlinks", nil},
		{"shell metacharacters rejected", "hyperlinks;rm -rf /", nil},
		{"flag injection rejected", "hyperlinks -f", nil},
		{"newline rejected", "hyperlinks\nsync", nil},
		{"overlong rejected", strings.Repeat("a", 129), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := TmuxClientFeatureArgs(tc.input)
			if len(got) != len(tc.want) {
				t.Fatalf("TmuxClientFeatureArgs(%q) = %v, want %v", tc.input, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("TmuxClientFeatureArgs(%q) = %v, want %v", tc.input, got, tc.want)
				}
			}
		})
	}
}

// A feature list of exactly the length limit must still pass: the bound exists to
// reject junk, and an off-by-one here would silently drop a legitimate opt-in.
func TestTmuxClientFeatureArgsAtLengthLimit(t *testing.T) {
	if got := TmuxClientFeatureArgs(strings.Repeat("a", 128)); len(got) != 2 {
		t.Fatalf("128-char feature list should be accepted; got %v", got)
	}
}
