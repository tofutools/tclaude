package agentd

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// dashboard_cost_factor.go backs the live cost-display multiplier on the
// Costs tab (and mirrors the Config tab's cost.estimate_factor field).
//
// The factor lives in config.json's "cost" block so it survives
// restarts and is shared with the Config tab. The full Config-tab save
// flow (dry-run → diff modal → confirm) would be heavy ceremony for a
// single number the human tweaks while watching the chart, so this is a
// deliberately small twin of /api/slop/volumes: GET returns the resolved
// factor, POST validates one value and merges it into the on-disk
// config. Dashboard-only, like /api/config — there is no /v1 twin; an
// agent has no business retuning the human's cost display.

// costFactorBody is the POST wire shape. EstimateFactor is a pointer so
// the client can clear the override (send null) as distinct from setting
// a value. For the global default, 1 (or null) clears it; a harness
// override of 1 is preserved, and only null restores inheritance.
// The global 1 is the no-op default,
// so we keep config.json tidy rather than persisting a redundant 1.
type costFactorBody struct {
	EstimateFactor *float64 `json:"estimate_factor"`
	Harness        string   `json:"harness,omitempty"`
	ResetAll       bool     `json:"reset_all,omitempty"`
}

// handleDashboardCostFactorAPI dispatches /api/cost-factor by method.
// Mounted on the loopback dashboard mux by registerDashboardEditRoutes.
func handleDashboardCostFactorAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		handleDashboardCostFactorGet(w)
	case http.MethodPost:
		handleDashboardCostFactorPost(w, r)
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// writeCostFactor returns the resolved (clamped, defaulted) factor so
// the client always renders a usable number even when the on-disk value
// is absent or out of range.
func writeCostFactor(w http.ResponseWriter, cfg *config.Config) {
	overrides := map[string]float64{}
	if cfg != nil && cfg.Cost != nil {
		for name := range cfg.Cost.HarnessFactors {
			if config.ValidCostFactorHarness(name) {
				overrides[name] = cfg.CostFactorForHarness(name)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"harness_factors": overrides,
		"estimate_factor": cfg.ResolvedCostFactor(),
	})
}

func handleDashboardCostFactorGet(w http.ResponseWriter) {
	// A malformed file degrades to defaults (factor 1) — fine for a read;
	// the write path below is the one that must not silently wipe it.
	cfg, _ := config.Load()
	writeCostFactor(w, cfg)
}

func handleDashboardCostFactorPost(w http.ResponseWriter, r *http.Request) {
	var body costFactorBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "request body is not valid JSON: "+err.Error())
		return
	}
	if body.Harness != "" && !config.ValidCostFactorHarness(body.Harness) {
		writeError(w, http.StatusBadRequest, "unknown_harness", "unknown cost multiplier harness")
		return
	}
	if body.ResetAll && (body.Harness != "" || body.EstimateFactor != nil) {
		writeError(w, http.StatusBadRequest, "bad_request", "reset_all cannot be combined with a harness or factor")
		return
	}

	// Reject an explicit out-of-range value up front so the human gets the
	// same guard the Config tab's Validate gives. A null / 1 clears the
	// override (handled below), so only a present, non-1 value is checked.
	if f := body.EstimateFactor; f != nil && *f != 1 && (*f <= 0 || *f > costFactorMax) {
		writeError(w, http.StatusBadRequest, "out_of_range",
			"estimate_factor must be > 0 and ≤ 10 — it is a display multiplier, e.g. 1.1 for +10%")
		return
	}

	// config.Update serializes the whole load-merge-save against other
	// in-process config writers (the Config tab, the slop sliders) so a
	// quick retune can never resurrect a stale config over a concurrent
	// save.
	cfg, err := config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			// Load fell back to defaults, so a save here would replace the
			// corrupt file with defaults-plus-factor — silently discarding
			// whatever it held. Refuse; the Config tab owns that recovery.
			return errCostConfigMalformed
		}
		if cfg.Cost == nil {
			cfg.Cost = &config.CostConfig{}
		}
		switch {
		case body.ResetAll:
			cfg.Cost.EstimateFactor = nil
			cfg.Cost.HarnessFactors = nil
		case body.Harness != "":
			if body.EstimateFactor == nil {
				delete(cfg.Cost.HarnessFactors, body.Harness)
			} else {
				if cfg.Cost.HarnessFactors == nil {
					cfg.Cost.HarnessFactors = map[string]float64{}
				}
				cfg.Cost.HarnessFactors[body.Harness] = *body.EstimateFactor
			}
		case body.EstimateFactor == nil || *body.EstimateFactor == 1:
			cfg.Cost.EstimateFactor = nil
		default:
			cfg.Cost.EstimateFactor = body.EstimateFactor
		}
		if cfg.Cost.Empty() {
			cfg.Cost = nil
		}

		return nil
	})
	if errors.Is(err, errCostConfigMalformed) {
		writeError(w, http.StatusConflict, "malformed_target",
			"config.json on disk is corrupt — fix or replace it via the Config tab before changing the cost factor")
		return
	}
	if err != nil {
		http.Error(w, "save config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeCostFactor(w, cfg)
}

// costFactorMax mirrors config's maxCostEstimateFactor (unexported
// there) for the endpoint's range guard. Kept in lockstep with that
// constant; the authoritative clamp still lives in ResolvedCostFactor.
const costFactorMax = 10.0

// errCostConfigMalformed aborts the Update when the on-disk file is
// corrupt — mapped to the 409 above rather than a 500.
var errCostConfigMalformed = errors.New("config.json is malformed")
