package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

type spawnHarnessDescriptor struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

type spawnHarnessPolicyView struct {
	Scope       string                   `json:"scope"`
	Group       string                   `json:"group,omitempty"`
	Harnesses   []spawnHarnessDescriptor `json:"harnesses"`
	Rules       []db.SpawnHarnessRule    `json:"rules"`
	GlobalRules []db.SpawnHarnessRule    `json:"global_rules,omitempty"`
}

func spawnHarnessDescriptors() []spawnHarnessDescriptor {
	out := make([]spawnHarnessDescriptor, 0, len(harness.Names()))
	for _, name := range harness.Names() {
		h, err := harness.ResolveSpawnable(name)
		if err != nil {
			continue
		}
		out = append(out, spawnHarnessDescriptor{Name: h.Name, DisplayName: h.DisplayName})
	}
	return out
}

func readSpawnHarnessPolicy(w http.ResponseWriter, groupID int64, groupName string) {
	rules, err := db.ListSpawnHarnessRules(groupID)
	if err != nil {
		http.Error(w, "load spawn harness policy: "+err.Error(), http.StatusInternalServerError)
		return
	}
	view := spawnHarnessPolicyView{
		Scope:     "global",
		Harnesses: spawnHarnessDescriptors(),
		Rules:     rules,
	}
	if groupID > 0 {
		view.Scope = "group"
		view.Group = groupName
		view.GlobalRules, err = db.ListSpawnHarnessRules(0)
		if err != nil {
			http.Error(w, "load global spawn harness policy: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, http.StatusOK, view)
}

func replaceSpawnHarnessPolicy(w http.ResponseWriter, r *http.Request, groupID int64, groupName string) {
	var body struct {
		Rules []db.SpawnHarnessRule `json:"rules"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		http.Error(w, "decode spawn harness policy: "+err.Error(), http.StatusBadRequest)
		return
	}
	for _, rule := range body.Rules {
		for field, name := range map[string]string{"source": rule.SourceHarness, "target": rule.TargetHarness} {
			if _, err := harness.ResolveSpawnable(strings.TrimSpace(name)); err != nil {
				http.Error(w, fmt.Sprintf("%s harness %q is not spawnable in this build", field, name), http.StatusBadRequest)
				return
			}
		}
	}
	if err := db.ReplaceSpawnHarnessRules(groupID, body.Rules); err != nil {
		http.Error(w, "save spawn harness policy: "+err.Error(), http.StatusBadRequest)
		return
	}
	readSpawnHarnessPolicy(w, groupID, groupName)
}

func handleDashboardGlobalSpawnHarnessPolicy(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		readSpawnHarnessPolicy(w, 0, "")
	case http.MethodPut:
		replaceSpawnHarnessPolicy(w, r, 0, "")
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleDashboardGroupSpawnHarnessPolicy(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	switch r.Method {
	case http.MethodGet:
		readSpawnHarnessPolicy(w, g.ID, g.Name)
	case http.MethodPut:
		replaceSpawnHarnessPolicy(w, r, g.ID, g.Name)
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
