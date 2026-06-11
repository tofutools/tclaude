package common

import "testing"

func TestValidateModel(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty omits", "", "", false},
		{"whitespace omits", "   ", "", false},
		{"fable", "fable", "fable", false},
		{"fable 1m", "fable[1m]", "fable[1m]", false},
		{"opus", "opus", "opus", false},
		{"opus 1m", "opus[1m]", "opus[1m]", false},
		{"sonnet", "sonnet", "sonnet", false},
		{"sonnet 1m", "sonnet[1m]", "sonnet[1m]", false},
		{"haiku", "haiku", "haiku", false},
		{"case-folded", "Opus", "opus", false},
		{"case-folded 1m", "Sonnet[1M]", "sonnet[1m]", false},
		{"trimmed and folded", "  HAIKU ", "haiku", false},
		{"opusplan", "opusplan", "opusplan", false},
		{"full model id", "claude-opus-4-8", "claude-opus-4-8", false},
		{"full model id 1m", "claude-fable-5[1m]", "claude-fable-5[1m]", false},
		{"full model id folded", "Claude-Fable-5", "claude-fable-5", false},
		{"dated full id", "claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001", false},
		{"unknown model", "gpt", "", true},
		{"near miss", "opusx", "", true},
		{"no haiku 1m", "haiku[1m]", "", true},
		{"bare claude- prefix", "claude-", "", true},
		{"1m only on the end", "claude-[1m]x", "", true},
		{"no shell metacharacters", "claude-$(rm -rf)", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateModel(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ValidateModel(%q): expected error, got nil (value %q)", tc.in, got)
				}
				if got != "" {
					t.Fatalf("ValidateModel(%q): expected empty value on error, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateModel(%q): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ValidateModel(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsValidModel(t *testing.T) {
	for _, m := range ValidModels {
		if !IsValidModel(m) {
			t.Errorf("IsValidModel(%q) = false, want true", m)
		}
	}
	// Full model IDs pass without being in the alias list.
	for _, id := range []string{"claude-fable-5", "claude-fable-5[1m]", "claude-sonnet-4-6"} {
		if !IsValidModel(id) {
			t.Errorf("IsValidModel(%q) = false, want true", id)
		}
	}
	// Case sensitivity and unknown models are not valid here — callers
	// normalise via ValidateModel before this is reached.
	for _, bad := range []string{"", "OPUS", "gpt", "Fable", "haiku[1m]", "claude-", "claudefoo"} {
		if IsValidModel(bad) {
			t.Errorf("IsValidModel(%q) = true, want false", bad)
		}
	}
}
