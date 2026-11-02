package agentd

import (
	"encoding/json"
	"log/slog"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// resolveCollabDefault stores features.collab once, the first time a daemon
// starts after Collab became on by default. Builds before that never stored
// false: turning Collab off deleted the key. So an absent key with Collab
// traces left behind (Steward settings, a Steward, boards) means the operator
// turned Collab on and then off again, and it stays off. An absent key with no
// traces means Collab was never used, and it turns on, which only shows UI.
// Either way the value is stored, so this decides only once; only that key is
// written. A failed read stores nothing and leaves the key for the next start;
// until then this daemon reads the unset key as off.
// It runs before the daemon serves anything, and cfg is updated to match.
func resolveCollabDefault(cfg *config.Config) {
	if cfg == nil || (cfg.Features != nil && cfg.Features.Collab != nil) {
		return
	}
	on, err := config.StoreCollabDefault(func() (bool, error) {
		traces, err := collabTraces(cfg)
		return !traces, err
	})
	if err != nil {
		// Fail closed: this run can't tell whether the operator had turned
		// Collab off, and on would re-open what they closed.
		config.SetCollabUnsetOff(true)
		slog.Warn("collab: store the default failed; off for this run, deciding next start", "error", err)
		return
	}
	if cfg.Features == nil {
		cfg.Features = &config.FeaturesConfig{}
	}
	cfg.Features.Collab = &on
	slog.Info("collab: stored features.collab", "on", on)
}

// collabTraces reports whether anything only Collab could have made exists.
func collabTraces(cfg *config.Config) (bool, error) {
	if cfg.Steward != nil {
		raw, err := json.Marshal(cfg.Steward)
		if err != nil {
			return false, err
		}
		if string(raw) != "{}" {
			return true, nil
		}
	}
	// The Steward's scribe group, not an operator's own group of that name.
	if g, err := db.GetAgentGroupByName(stewardGroupName); err != nil || (g != nil && isScribeGroup(g)) {
		return err == nil, err
	}
	if roster, err := db.ListStewardRoster(); err != nil || len(roster) > 0 {
		return len(roster) > 0, err
	}
	if carried, err := db.ListStewardCarriedPermissions(); err != nil || len(carried) > 0 {
		return len(carried) > 0, err
	}
	if stewards, err := db.ListExternalStewards(); err != nil || len(stewards) > 0 {
		return len(stewards) > 0, err
	}
	boards, err := db.ListCollabBoards()
	if err != nil {
		return false, err
	}
	return len(boards) > 0, nil
}
