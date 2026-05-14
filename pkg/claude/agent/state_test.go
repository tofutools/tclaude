package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseStateFilter(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantOnline  bool
		wantApplies bool
		wantErr     bool
	}{
		{"empty -> no filter", "", false, false, false},
		{"online", "online", true, true, false},
		{"offline", "offline", false, true, false},
		{"online uppercase", "ONLINE", true, true, false},
		{"online with whitespace", "  online  ", true, true, false},
		{"unknown value", "foo", false, false, true},
		{"empty after trim is fine", "   ", false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotOnline, gotApplies, err := parseStateFilter(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantOnline, gotOnline, "online")
			assert.Equal(t, tt.wantApplies, gotApplies, "applies")
		})
	}
}

func TestParseDurationDays(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Duration
		bad  bool
	}{
		{"days", "30d", 30 * 24 * time.Hour, false},
		{"weeks", "2w", 2 * 7 * 24 * time.Hour, false},
		{"hours via stdlib", "12h", 12 * time.Hour, false},
		{"minutes via stdlib", "90m", 90 * time.Minute, false},
		{"single char d alone is invalid", "d", 0, true},
		{"empty", "", 0, true},
		{"garbage", "30x", 0, true},
		{"zero days ok", "0d", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDurationDays(tt.in)
			if tt.bad {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
