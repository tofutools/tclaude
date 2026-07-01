package agentd

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

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

	// config.Update serializes the whole load-merge-save against other
	// in-process config writers (the Config tab, permission defaults,
	// a second dashboard tab's slider) so a slider drag can never
	// resurrect a stale config over a concurrent save.
	cfg, err := config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			// Load fell back to defaults, so a save here would replace
			// the corrupt file with defaults-plus-volumes — silently
			// discarding whatever it held. Refuse; the Config tab owns
			// that recovery.
			return errSlopConfigMalformed
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
		return nil
	})
	if errors.Is(err, errSlopConfigMalformed) {
		writeError(w, http.StatusConflict, "malformed_target",
			"config.json on disk is corrupt — fix or replace it via the Config tab before changing volumes")
		return
	}
	if err != nil {
		http.Error(w, "save config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeSlopVolumes(w, cfg)
}

// errSlopConfigMalformed aborts the Update when the on-disk file is
// corrupt — mapped to the 409 above rather than a 500.
var errSlopConfigMalformed = errors.New("config.json is malformed")

// ─── Channel picker ────────────────────────────────────────────────────
// The Vegas radio's SomaFM channel is a slop setting like the volumes, so
// it lives in the same config.json "slop" block and is read/written by the
// same small twin pattern: GET resolves the current channel (+ the catalog
// the picker renders), POST validates against the allowlist and merge-saves.

// slopChannelBody is the POST wire shape — just the chosen channel id.
type slopChannelBody struct {
	Channel string `json:"channel"`
}

// handleDashboardSlopChannelAPI dispatches /api/slop/channel by method.
// Mounted on the loopback dashboard mux by registerDashboardEditRoutes.
func handleDashboardSlopChannelAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		handleDashboardSlopChannelGet(w)
	case http.MethodPost:
		handleDashboardSlopChannelPost(w, r)
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// writeSlopChannel returns the resolved current channel plus the catalog
// the picker renders (id + human label), so the browser need not hardcode
// either — the allowlist stays single-sourced in config.SlopChannels.
//
// `persisted` tells the browser whether `channel` is a deliberate saved
// choice or just the default fallback: a fresh listener (persisted=false)
// hears the active theme's default station (the wizard Tavern vs the Vegas
// lounge), while an explicit pick is honored across both themes. See
// js/vegas.js loadChannel.
func writeSlopChannel(w http.ResponseWriter, cfg *config.Config) {
	writeJSON(w, http.StatusOK, map[string]any{
		"channel":   cfg.ResolvedSlopChannel(),
		"channels":  config.SlopChannels,
		"persisted": cfg.HasExplicitSlopChannel(),
	})
}

func handleDashboardSlopChannelGet(w http.ResponseWriter) {
	// A malformed file degrades to the default channel — fine for a read;
	// the write path below is the one that must not wipe it.
	cfg, _ := config.Load()
	writeSlopChannel(w, cfg)
}

func handleDashboardSlopChannelPost(w http.ResponseWriter, r *http.Request) {
	var body slopChannelBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "request body is not valid JSON: "+err.Error())
		return
	}
	channel := strings.TrimSpace(body.Channel)
	// Reject an unknown channel rather than silently saving it (the proxy
	// would degrade it to the default, but a stored value that doesn't match
	// what plays would be confusing). The allowlist is the SSRF gate too.
	if !config.IsKnownSlopChannel(channel) {
		writeError(w, http.StatusBadRequest, "unknown_channel",
			"channel must be one of: "+strings.Join(config.SlopChannels, ", "))
		return
	}

	cfg, err := config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			// Same reasoning as the volumes path: don't overwrite a corrupt
			// file with defaults-plus-channel. The Config tab owns recovery.
			return errSlopConfigMalformed
		}
		if cfg.Slop == nil {
			cfg.Slop = &config.SlopConfig{}
		}
		cfg.Slop.Channel = &channel
		return nil
	})
	if errors.Is(err, errSlopConfigMalformed) {
		writeError(w, http.StatusConflict, "malformed_target",
			"config.json on disk is corrupt — fix or replace it via the Config tab before changing the channel")
		return
	}
	if err != nil {
		http.Error(w, "save config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeSlopChannel(w, cfg)
}
