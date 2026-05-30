package common

import "testing"

func TestValidateEffort(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty omits", "", "", false},
		{"whitespace omits", "   ", "", false},
		{"low", "low", "low", false},
		{"medium", "medium", "medium", false},
		{"high", "high", "high", false},
		{"xhigh", "xhigh", "xhigh", false},
		{"max", "max", "max", false},
		{"case-folded", "High", "high", false},
		{"trimmed and folded", "  MAX ", "max", false},
		{"unknown level", "ultra", "", true},
		{"near miss", "highest", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateEffort(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ValidateEffort(%q): expected error, got nil (value %q)", tc.in, got)
				}
				if got != "" {
					t.Fatalf("ValidateEffort(%q): expected empty value on error, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateEffort(%q): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ValidateEffort(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsValidEffort(t *testing.T) {
	for _, lvl := range ValidEffortLevels {
		if !IsValidEffort(lvl) {
			t.Errorf("IsValidEffort(%q) = false, want true", lvl)
		}
	}
	// Case sensitivity and unknown levels are not valid here — callers
	// normalise via ValidateEffort before this is reached.
	for _, bad := range []string{"", "LOW", "ultra", "hi", "Max"} {
		if IsValidEffort(bad) {
			t.Errorf("IsValidEffort(%q) = true, want false", bad)
		}
	}
}
