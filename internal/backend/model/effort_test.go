package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestedEffortCannotBecomeFlagsOrChangeLegacyJSON(t *testing.T) {
	for _, value := range []string{"--model", " high", "high\n", "x\"", strings.Repeat("x", 65)} {
		if ValidateEffort(value) == nil {
			t.Fatalf("accepted malformed effort %q", value)
		}
	}
	for _, value := range []string{"", "low", "high", "custom-variant_2"} {
		if err := ValidateEffort(value); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(DesiredConfiguration{Harness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Effort") {
		t.Fatal("empty effort changes existing configuration JSON and hashes")
	}
}
