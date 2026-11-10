package conv

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// TestMain keeps a resumed no-sandbox Codex launch from asking a real Codex
// for its managed requirements: tests report none.
func TestMain(m *testing.M) {
	restore := session.SetCodexRequirementsProbeForTest(
		func(string, []sandboxpolicy.EnvironmentEntry) (json.RawMessage, error) {
			return json.RawMessage(`{"requirements":null}`), nil
		})
	code := m.Run()
	restore()
	os.Exit(code)
}
