package agentd

import (
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// migrateStateIntoDataDir relocates pre-split daemon-owned state that lives
// directly under ~/.tclaude into ~/.tclaude/data, so the data/ sandbox deny
// covers it as one subtree.
//
// The shared config migration also runs from the CLI pre-run path, because
// config.json, processes/, and remote-access/ have non-daemon writers that must
// not create empty destinations before the daemon restarts. The SQLite group
// still has its own ordered relocation in db.Open. scribe/ is deliberately NOT
// moved: it is an agent-facing, cwd-bound workdir that must stay writable and
// reachable, hence outside the denied data/.
//
// serve.go repeats this after prepareSocketPath has rejected an already-running
// daemon. Usually the CLI pre-run already completed it; the second call is an
// idempotent safety net immediately before db.Open.
func migrateStateIntoDataDir() error {
	return config.RelocateLegacyState()
}
