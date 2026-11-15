package engine

import (
	"reflect"
	"testing"
)

// agentCommandsEqual lists AgentCommand's fields by hand (the struct holds
// slices, so == cannot compare it). A field it misses would let a run
// re-prepare to a different command unnoticed: this fails until the new
// field is compared there, and the count bumped here.
func TestAgentCommandsEqualCoversEveryField(t *testing.T) {
	if got := reflect.TypeFor[AgentCommand]().NumField(); got != 15 {
		t.Fatalf("AgentCommand has %d fields: compare the new one in agentCommandsEqual, then update this count", got)
	}
	base := AgentCommand{DifferentFrom: []string{"a"}, Captures: []string{"plan"}}
	for name, edit := range map[string]func(*AgentCommand){
		"DifferentFrom": func(c *AgentCommand) { c.DifferentFrom = []string{"b"} },
		"Captures":      func(c *AgentCommand) { c.Captures = nil },
		"SameAs":        func(c *AgentCommand) { c.SameAs = "x" },
		"Lead":          func(c *AgentCommand) { c.Lead = true },
		"RetryMode":     func(c *AgentCommand) { c.RetryMode = "x" },
	} {
		changed := base
		edit(&changed)
		if agentCommandsEqual(base, changed) {
			t.Errorf("a different %s compares equal", name)
		}
	}
	if !agentCommandsEqual(base, AgentCommand{DifferentFrom: []string{"a"}, Captures: []string{"plan"}}) {
		t.Error("equal commands compare different")
	}
}
