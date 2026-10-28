package setup

import (
	"errors"
	"slices"
	"testing"
)

func TestMissingLinuxSandboxPackages(t *testing.T) {
	cases := []struct {
		name    string
		present []string
		want    []string
	}{
		{"all present", []string{"bwrap", "socat"}, nil},
		{"socat missing", []string{"bwrap"}, []string{"socat"}},
		{"both missing", nil, []string{"bubblewrap", "socat"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := lookPathSandboxTool
			t.Cleanup(func() { lookPathSandboxTool = orig })
			lookPathSandboxTool = func(bin string) (string, error) {
				if slices.Contains(tc.present, bin) {
					return "/usr/bin/" + bin, nil
				}
				return "", errors.New("not found")
			}
			if got := missingLinuxSandboxPackages(); !slices.Equal(got, tc.want) {
				t.Fatalf("missing = %v, want %v", got, tc.want)
			}
		})
	}
}
