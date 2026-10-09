package agentd

import (
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/noderun"
)

// SetNodeRunExecutorForTest swaps only the external execution boundary.
func SetNodeRunExecutorForTest(execute noderun.Execute) (func(), error) {
	stopNodeRuns()
	dir := filepath.Join(common.TclaudeDataDir(), "node-runs")
	svc, err := noderun.New(dir, execute, func(j noderun.Job) { recordNodeRunAudit(j, "result") })
	if err != nil {
		return nil, err
	}
	nodeRuns.Lock()
	nodeRuns.dir, nodeRuns.service = dir, svc
	nodeRuns.Unlock()
	return stopNodeRuns, nil
}
