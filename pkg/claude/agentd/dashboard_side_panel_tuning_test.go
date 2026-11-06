package agentd

import (
	"io/fs"
	"regexp"
	"strconv"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// The Config tab's side panel tuning fields (js/side-panel-tuning-fields.js)
// carry the same defaults and ranges the daemon clamps to
// (config.SidePanelTuningLimits), so the form never shows a default or offers
// a value the dashboard would not run. side-panel-reveal.test.mjs pins the
// same defaults against the dashboard's own constants.
func TestDashboardSidePanelTuningFieldsMatchConfig(t *testing.T) {
	src, err := fs.ReadFile(dashboardAssetsFS, "js/side-panel-tuning-fields.js")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`key: '(\w+)'.*def: (\d+), min: (\d+), max: (\d+)`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		key := m[1]
		limit, ok := config.SidePanelTuningLimits[key]
		if !ok {
			t.Errorf("field %s has no config.SidePanelTuningLimits entry", key)
			continue
		}
		def, _ := strconv.Atoi(m[2])
		lo, _ := strconv.Atoi(m[3])
		hi, _ := strconv.Atoi(m[4])
		if (config.SidePanelTuningLimit{Default: def, Min: lo, Max: hi}) != limit {
			t.Errorf("field %s: form {%d %d %d}, config %+v", key, def, lo, hi, limit)
		}
		seen[key] = true
	}
	for key := range config.SidePanelTuningLimits {
		if !seen[key] {
			t.Errorf("config.SidePanelTuningLimits[%s] has no Config tab field", key)
		}
	}
}
