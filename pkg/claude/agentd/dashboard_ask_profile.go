package agentd

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// dashboard_ask_profile.go backs the Config tab's "Ask defaults" section
// (ask-profile.js) — the persistent model/effort profile for `tclaude
// ask` (JOH-253).
//
// The profile lives in config.json's "ask" block (config.AskConfig), the
// same single source of truth the `tclaude ask` CLI reads in
// PersistentPreRun — the dashboard is a thin editor over it, not a second
// store. This is a deliberately small twin of /api/cost-factor: GET
// returns the resolved profile plus the harness catalog (so the selectors
// render without a hardcoded model list), POST validates model/effort
// against the harness catalog and merges them into config.json. Empty /
// null clears a field, which resolves back to the fast-by-default
// constant. Dashboard-only, like /api/cost-factor — there is no /v1 twin;
// the ask profile is the human's terminal preference, not an agent knob.
//
// Validation uses the DEFAULT harness catalog (Claude Code). The ask
// profile is global and only Claude ask is wired today; a future Codex
// ask (JOH-252) would not change the schema, only widen the accepted set.

// askProfileBody is the POST wire shape. Both fields are plain strings:
// the form always sends both, and an empty value clears that field (so it
// resolves to the fast default). A field omitted from the JSON decodes to
// "" — also a clear — which is the safe default for this form.
type askProfileBody struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

// errAskConfigMalformed aborts the POST Update when the on-disk config is
// corrupt — mapped to a 409 rather than silently replacing it (the
// Config tab owns that recovery), matching /api/cost-factor.
var errAskConfigMalformed = errors.New("config.json is malformed")

// handleDashboardAskProfileAPI dispatches /api/ask-profile by method.
// Mounted on the loopback dashboard mux by registerDashboardEditRoutes.
func handleDashboardAskProfileAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		handleDashboardAskProfileGet(w)
	case http.MethodPost:
		handleDashboardAskProfilePost(w, r)
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// writeAskProfile returns the resolved (defaulted) profile plus the
// "set" flags and the harness catalog, so the client always renders a
// usable selector even when config.json pins nothing.
func writeAskProfile(w http.ResponseWriter, cfg *config.Config) {
	model, effort := cfg.ResolvedAskProfile()
	h := harness.Default()
	var models, efforts []string
	if h.Models != nil {
		models = h.Models.Models()
		efforts = h.Models.EffortLevels()
	}
	// "set" = config.json explicitly pins the field (vs falling back to
	// the fast default), so the UI can show "(fast default)" as selected
	// rather than the resolved alias.
	modelSet := cfg != nil && cfg.Ask != nil && cfg.Ask.Model != ""
	effortSet := cfg != nil && cfg.Ask != nil && cfg.Ask.Effort != ""
	writeJSON(w, http.StatusOK, map[string]any{
		"model":          model,
		"effort":         effort,
		"model_set":      modelSet,
		"effort_set":     effortSet,
		"default_model":  config.DefaultAskModel,
		"default_effort": config.DefaultAskEffort,
		"models":         models,
		"efforts":        efforts,
	})
}

func handleDashboardAskProfileGet(w http.ResponseWriter) {
	// A malformed file degrades to defaults — fine for a read; the write
	// path below is the one that must not silently wipe a corrupt file.
	cfg, _ := config.Load()
	writeAskProfile(w, cfg)
}

func handleDashboardAskProfilePost(w http.ResponseWriter, r *http.Request) {
	var body askProfileBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "request body is not valid JSON: "+err.Error())
		return
	}

	// Validate against the default harness catalog up front so the human
	// gets the same guard the CLI would. Empty stays empty (= clear the
	// field); a non-empty value is normalized (trimmed/case-folded) to the
	// token we persist.
	h := harness.Default()
	model, effort := strings.TrimSpace(body.Model), strings.TrimSpace(body.Effort)
	if h.Models != nil {
		var err error
		if model, err = h.Models.ValidateModel(model); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_model", err.Error())
			return
		}
		if effort, err = h.Models.ValidateEffort(effort); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_effort", err.Error())
			return
		}
	}

	// config.Update serializes the load-merge-save against other in-process
	// config writers (the Config tab, the cost/slop endpoints) so a quick
	// edit can never resurrect a stale config over a concurrent save.
	cfg, err := config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			// Load fell back to defaults, so a save here would replace the
			// corrupt file with defaults-plus-profile — discarding whatever
			// it held. Refuse; the Config tab owns that recovery.
			return errAskConfigMalformed
		}
		if model == "" && effort == "" {
			// Both cleared: drop the whole block so config.json doesn't
			// accrue a redundant empty "ask": {}.
			cfg.Ask = nil
			return nil
		}
		if cfg.Ask == nil {
			cfg.Ask = &config.AskConfig{}
		}
		cfg.Ask.Model = model
		cfg.Ask.Effort = effort
		return nil
	})
	if errors.Is(err, errAskConfigMalformed) {
		writeError(w, http.StatusConflict, "malformed_target",
			"config.json on disk is corrupt — fix or replace it via the Config tab before changing the ask profile")
		return
	}
	if err != nil {
		http.Error(w, "save config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeAskProfile(w, cfg)
}
