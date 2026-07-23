package agent

import (
	"bytes"
	"strings"
	"testing"
)

// JOH-351: the `roles show` / `roles ls` renderers. Pure unit tests over the
// wire-shape roleJSON — no daemon — locking in the transparency floor a
// terminal-driven operator relies on (the CLI twin of the dashboard's
// role-inspect panel). Mirrors task_force_readback_test.go's style.

// TestRoleLaunchSummary covers the ls table's compact launch cell: a spawn
// profile leads (prefixed "@"), else the model, else the harness, else "—".
func TestRoleLaunchSummary(t *testing.T) {
	cases := []struct {
		name string
		rl   roleJSON
		want string
	}{
		{"profile wins over model+harness", roleJSON{SpawnProfile: "prof", Model: "opus", Harness: "codex"}, "@prof"},
		{"model when no profile", roleJSON{Model: "opus", Harness: "codex"}, "opus"},
		{"harness when only harness", roleJSON{Harness: "codex"}, "codex"},
		{"dash when nothing set", roleJSON{}, "—"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := roleLaunchSummary(c.rl); got != c.want {
				t.Fatalf("roleLaunchSummary(%+v) = %q, want %q", c.rl, got, c.want)
			}
		})
	}
}

// TestPrintRoleHuman_FullRole asserts every field a set role carries surfaces in
// the human view: name, descr, the launch fields in stable order, the perms, and
// the multi-line brief indented under a "brief:" header.
func TestPrintRoleHuman_FullRole(t *testing.T) {
	var buf bytes.Buffer
	printRoleHuman(&buf, roleJSON{
		Name:           "reviewer",
		Descr:          "cold reviewer",
		SpawnProfile:   "sandboxed",
		Harness:        "codex",
		Model:          "opus",
		Effort:         "high",
		Sandbox:        "read-only",
		Approval:       "on-request",
		ToolGovernance: "deny",
		Permissions:    []string{"human.notify", "agent.rename"},
		Brief:          "You review with fresh eyes.\nBe skeptical.",
	})
	out := buf.String()

	for _, want := range []string{
		"Role: reviewer",
		"descr:   cold reviewer",
		// Launch fields render profile-first then the stable inline order.
		"launch:  profile=sandboxed · harness=codex · model=opus · effort=high · sandbox=read-only · approval=on-request · tools=deny",
		"perms:   human.notify, agent.rename",
		"  brief:",
		"    You review with fresh eyes.",
		"    Be skeptical.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("printRoleHuman output missing %q\n--- got ---\n%s", want, out)
		}
	}
}

// TestPrintRoleHuman_MinimalRole asserts an all-blank role prints just its name —
// no empty "launch:" / "perms:" / "brief:" lines that would imply defaults it
// doesn't carry.
func TestPrintRoleHuman_MinimalRole(t *testing.T) {
	var buf bytes.Buffer
	printRoleHuman(&buf, roleJSON{Name: "bare"})
	out := buf.String()

	if !strings.Contains(out, "Role: bare") {
		t.Fatalf("missing name line: %q", out)
	}
	for _, absent := range []string{"descr:", "launch:", "perms:", "brief:"} {
		if strings.Contains(out, absent) {
			t.Errorf("bare role should not render %q line: %q", absent, out)
		}
	}
}
