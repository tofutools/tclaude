package agentd

import (
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// Spawner abstracts `tclaude session new` invocations so flow tests
// can substitute a behavior-accurate simulator for the real
// fork-exec subprocess. Production wires LiveSpawner; tests assign
// a fake to Spawn at setup with t.Cleanup-restoration.
//
// Both methods take a clcommon.SpawnArgs (named fields) rather than a long
// positional list — see that type for the per-field semantics (harness,
// sandbox, approval, auto-review, trust-dir, etc.). SpawnNew launches a fresh
// conversation keyed by SpawnArgs.Label; SpawnResume relaunches the
// conversation named by SpawnArgs.ConvID and ignores the fresh-spawn-only
// Label / TrustDir fields.
type Spawner interface {
	SpawnNew(args clcommon.SpawnArgs) error
	SpawnResume(args clcommon.SpawnArgs) error
}

// Spawn is the package-wide Spawner every caller hits via the
// SpawnDetachedTclaude{New,Resume} facades. Production starts on
// LiveSpawner; tests overwrite during their setup. Single global var =
// goroutine-unsafe across parallel tests on the same package — flow
// tests don't t.Parallel.
var Spawn Spawner = LiveSpawner{}

// LiveSpawner is the production impl: forks `tclaude session new -d`
// for spawn, `-r <conv> -d` for resume. Exported so tests can wrap
// it (e.g., a recording proxy).
type LiveSpawner struct{}

func (LiveSpawner) SpawnNew(args clcommon.SpawnArgs) error {
	return liveSpawnNew(args)
}
func (LiveSpawner) SpawnResume(args clcommon.SpawnArgs) error {
	return liveSpawnResume(args)
}
