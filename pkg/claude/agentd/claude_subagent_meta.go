package agentd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// claudeSubagentMetaMaxBytes caps how much of a sub-agent's meta sidecar is
// read. The file is a few hundred bytes; anything larger is not one.
const claudeSubagentMetaMaxBytes = 64 << 10

// claudeFinishedSubagents returns the ledger ids whose Claude Code sub-agent
// meta sidecar (<project>/<conv>/subagents/agent-<id>.meta.json) records
// "finished": true.
//
// The hook stream is lossy about sub-agent ends: a SubagentStop can be lost,
// or carry an id that matches nothing, and a sub-agent can end in a way the
// hook callback does not recognise. Claude Code writes the sidecar when the
// sub-agent ends, whatever hooks fired, so it is authoritative for that one
// terminal fact, the same way a Codex rollout is for an interrupted child.
// Like that backstop it is read-through only: the drop is applied to what
// the read shows, and the stored ledger converges on the session's next
// hook, or on TTL expiry.
//
// A missing, unreadable or unparsable sidecar keeps the entry: absence is
// not evidence that the sub-agent ended, and the TTL still applies.
func claudeFinishedSubagents(pick *db.SessionRow, set db.SubagentSet) map[string]struct{} {
	if pick == nil || len(set) == 0 || pick.ConvID == "" || pick.Cwd == "" {
		return nil
	}
	if pick.Harness != "" && pick.Harness != harness.DefaultName {
		return nil
	}
	dir := filepath.Join(convops.GetClaudeProjectPath(pick.Cwd), pick.ConvID, "subagents")
	var finished map[string]struct{}
	for id := range set {
		if !claudeSubagentIDSafe(id) || !claudeSubagentMetaFinished(filepath.Join(dir, "agent-"+id+".meta.json")) {
			continue
		}
		if finished == nil {
			finished = map[string]struct{}{}
		}
		finished[id] = struct{}{}
	}
	return finished
}

// claudeSubagentIDSafe keeps a ledger id (hook input) from naming a path
// outside the subagents directory.
func claudeSubagentIDSafe(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func claudeSubagentMetaFinished(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, claudeSubagentMetaMaxBytes))
	if err != nil {
		return false
	}
	var meta struct {
		Finished bool `json:"finished"`
	}
	if json.Unmarshal(data, &meta) != nil {
		return false
	}
	return meta.Finished
}
