package agentd

import (
	"encoding/json"
	"log/slog"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
)

// keepImportedExtensions stores the extension data of an imported file with
// the landed agent. Best effort, like the rest of what follows a landing.
func keepImportedExtensions(spawned agent.SpawnResponse, c *paf.CarriedExtensions) {
	if c == nil {
		return
	}
	agentID := spawned.AgentID
	if agentID == "" {
		agentID, _ = db.AgentIDForConv(spawned.ConvID)
	}
	raw, err := json.Marshal(c)
	if err == nil {
		err = db.SetAgentPAFExtensions(agentID, raw)
	}
	if err != nil {
		slog.Warn("import: keep extension data", "agent", agentID, "error", err)
	}
}

// carriedExtensions is what an export of conv writes back.
func carriedExtensions(conv string) *paf.CarriedExtensions {
	agentID, err := db.AgentIDForConv(conv)
	if err != nil || agentID == "" {
		return nil
	}
	raw, err := db.AgentPAFExtensions(agentID)
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
	return c
}

// scanCarriedExtensions adds c to an export's credential scan.
func scanCarriedExtensions(s *privacy.Collector, c *paf.CarriedExtensions) {
	if c == nil {
		return
	}
	if raw, err := json.Marshal(map[string]any{"decls": c.Decls, "manifest": c.Manifest, "agent": c.Agent, "environment": c.Environment, "lines": c.Lines}); err == nil {
		s.JSON(raw, "extensions")
	}
	for _, e := range c.Entries {
		s.Text(string(e.Data), e.Path)
	}
}
