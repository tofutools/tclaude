package agent

import (
	"bytes"
	"strings"
	"testing"
)

func TestNonInteractiveSpawnRequiresExplicitPrompt(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, code := RunSpawn(&SpawnParams{Group: "test", NonInteractive: true}, &stdout, &stderr, strings.NewReader(""))
	if code != rcInvalidArg || !strings.Contains(stderr.String(), "requires --initial-message or --file") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestNonInteractiveSpawnRejectsInapplicableFlags(t *testing.T) {
	for _, option := range []struct {
		name  string
		apply func(*SpawnParams)
	}{
		{"reply-to", func(p *SpawnParams) { p.ReplyTo = "peer" }},
		{"auto-focus", func(p *SpawnParams) { p.AutoFocus = true }},
		{"ask-for-approval", func(p *SpawnParams) { p.Approval = "never" }},
		{"owner", func(p *SpawnParams) { p.Owner = true }},
		{"codex-app-server", func(p *SpawnParams) { p.CodexAppServer = true }},
	} {
		t.Run(option.name, func(t *testing.T) {
			p := &SpawnParams{Group: "test", NonInteractive: true, InitialMessage: "hello"}
			option.apply(p)
			var stdout, stderr bytes.Buffer
			_, code := RunSpawn(p, &stdout, &stderr, strings.NewReader(""))
			if code != rcInvalidArg || !strings.Contains(stderr.String(), "--"+option.name+" is not supported") {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
		})
	}
}
