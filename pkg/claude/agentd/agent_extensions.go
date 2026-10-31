package agentd

import (
	"encoding/json"
	"log/slog"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
)

// keepImportedExtensions stores the extension data of an imported file that
// tclaude does not understand with the landed agent. Extensions declared
// copy: unsafe stay only when the agent lands offline with its history as it
// was: a translated landing has changed what they describe, and a started
// agent is about to. Best effort, like the rest of what follows a landing.
func keepImportedExtensions(spawned agent.SpawnResponse, c *paf.CarriedExtensions, h *harness.Harness, offline bool, l *historyLanding, cwd string) {
	if c == nil {
		return
	}
	agentID := spawned.AgentID
	if agentID == "" {
		agentID, _ = db.AgentIDForConv(spawned.ConvID)
	}
	digest := ""
	if offline && l.Rung == landingNative {
		b := &agentbundle.Bundle{MaxBytes: agentTransferLimit()}
		defer func() { _ = b.Close() }()
		if err := snapshotAgentHistory(b, h, spawned.ConvID, cwd); err != nil {
			slog.Warn("import: digest landed history", "agent", agentID, "error", err)
		} else if b.Manifest.History != nil {
			digest = b.Manifest.History.SHA256
		}
	}
	if digest == "" {
		c = c.SafeOnly()
	}
	if c == nil {
		return
	}
	raw, err := json.Marshal(c)
	if err == nil {
		err = db.SetAgentPAFExtensions(agentID, raw, digest)
	}
	if err != nil {
		slog.Warn("import: keep extension data", "agent", agentID, "error", err)
	}
}

// carriedExtensions is what an export of conv writes back: everything while
// the history is the one that landed, otherwise only copy: safe extensions.
func carriedExtensions(conv string, b *agentbundle.Bundle) *paf.CarriedExtensions {
	agentID, err := db.AgentIDForConv(conv)
	if err != nil || agentID == "" {
		return nil
	}
	raw, digest, err := db.AgentPAFExtensions(agentID)
	if err != nil {
		slog.Warn("paf: read extension data", "agent", agentID, "error", err)
		return nil
	}
	if raw == nil {
		return nil
	}
	c := &paf.CarriedExtensions{}
	if err := json.Unmarshal(raw, c); err != nil {
		slog.Warn("paf: decode extension data", "agent", agentID, "error", err)
		return nil
	}
	if b.Manifest.History == nil || digest == "" || b.Manifest.History.SHA256 != digest {
		return c.SafeOnly()
	}
	return c
}

// scanCarriedExtensions adds c to an export's credential scan.
func scanCarriedExtensions(s *privacy.Collector, c *paf.CarriedExtensions) {
	if c == nil {
		return
	}
	if raw, err := json.Marshal(map[string]any{"manifest": c.Manifest, "agent": c.Agent, "environment": c.Environment, "lines": c.Lines}); err == nil {
		s.JSON(raw, "extensions")
	}
	for _, e := range c.Entries {
		s.Text(string(e.Data), e.Path)
	}
}
