package agentd

import (
	"encoding/json"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// dashboard_slop.go backs the slop-mode volume sliders (the 🎚️ popover
// next to the header's 🔇/🔊 master switch — see slop-volume.js).
//
// The values live in config.json's "slop" block so they survive
// restarts and machines that share a config. The full Config-tab save
// flow (dry-run → diff modal → confirm) would be absurd ceremony for a
// volume slider, so this is a deliberately small twin: GET returns the
// resolved volumes, POST merges the submitted ones into the on-disk
// config and saves. Dashboard-only, like /api/config: there is no /v1
// twin — agents have no business mixing the casino.

// slopVolumesBody is the POST wire shape. Pointers so a caller can set
// one volume without clobbering the other (absent key = leave as-is).
type slopVolumesBody struct {
	MusicVolume   *int `json:"music_volume"`
	EffectsVolume *int `json:"effects_volume"`
}

// handleDashboardSlopVolumesAPI dispatches /api/slop/volumes by method.
// Mounted on the loopback dashboard mux by registerDashboardEditRoutes.
func handleDashboardSlopVolumesAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		handleDashboardSlopVolumesGet(w)
	case http.MethodPost:
		handleDashboardSlopVolumesPost(w, r)
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

func writeSlopVolumes(w http.ResponseWriter, cfg *config.Config) {
	music, effects := cfg.ResolvedSlopVolumes()
	writeJSON(w, http.StatusOK, map[string]any{
		"music_volume":   music,
		"effects_volume": effects,
	})
}

func handleDashboardSlopVolumesGet(w http.ResponseWriter) {
	// A malformed file degrades to defaults (100/100) — fine for a
	// read; the write path below is the one that must not wipe it.
	cfg, _ := config.Load()
	writeSlopVolumes(w, cfg)
}

func handleDashboardSlopVolumesPost(w http.ResponseWriter, r *http.Request) {
	var body slopVolumesBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "request body is not valid JSON: "+err.Error())
		return
	}
	if body.MusicVolume == nil && body.EffectsVolume == nil {
		writeError(w, http.StatusBadRequest, "empty_body", "set music_volume and/or effects_volume (0–100)")
		return
	}
	for _, v := range []*int{body.MusicVolume, body.EffectsVolume} {
		if v != nil && (*v < 0 || *v > 100) {
			writeError(w, http.StatusBadRequest, "out_of_range", "volumes must be 0–100")
			return
		}
	}

	cfg, loadErr := config.Load()
	if loadErr != nil {
		// Load fell back to defaults, so a save here would replace the
		// corrupt file with defaults-plus-volumes — silently discarding
		// whatever it held. Refuse; the Config tab owns that recovery.
		writeError(w, http.StatusConflict, "malformed_target",
			"config.json on disk is corrupt — fix or replace it via the Config tab before changing volumes")
		return
	}
	if cfg.Slop == nil {
		cfg.Slop = &config.SlopConfig{}
	}
	if body.MusicVolume != nil {
		cfg.Slop.MusicVolume = body.MusicVolume
	}
	if body.EffectsVolume != nil {
		cfg.Slop.EffectsVolume = body.EffectsVolume
	}
	if err := config.Save(cfg); err != nil {
		http.Error(w, "save config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeSlopVolumes(w, cfg)
}
