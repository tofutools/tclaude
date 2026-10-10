package hub

import (
	"fmt"
	"slices"
	"time"
)

type Setting struct {
	Type            string `json:"type"`
	Unit            string `json:"unit"`
	Min             int64  `json:"min"`
	Max             int64  `json:"max"`
	Effective       int64  `json:"effective"`
	Source          string `json:"source"`
	Boot            int64  `json:"boot"`
	RestartRequired bool   `json:"restart_required"`
	FlagOverridden  bool   `json:"flag_overridden"`
}

var settingSpecs = map[string]Setting{
	"identity_rotation_window_seconds": {Type: "integer", Unit: "seconds", Min: 1, Max: 86400},
	"frames_per_minute":                {Type: "integer", Unit: "frames/minute", Min: 1, Max: 100000},
	"bytes_per_minute":                 {Type: "integer", Unit: "bytes/minute", Min: 1024, Max: 1 << 30},
	"max_connections":                  {Type: "integer", Unit: "connections", Min: 1, Max: 100000},
	"max_streams":                      {Type: "integer", Unit: "streams/instance", Min: 1, Max: 1024},
	"stream_bytes_per_second":          {Type: "integer", Unit: "bytes/second", Min: 1024, Max: 1 << 30},
	"stream_wait_seconds":              {Type: "integer", Unit: "seconds", Min: 1, Max: 600},
	"stream_idle_seconds":              {Type: "integer", Unit: "seconds", Min: 30, Max: 3600},
	"connection_idle_seconds":          {Type: "integer", Unit: "seconds", Min: 30, Max: 3600},
	"hello_timeout_seconds":            {Type: "integer", Unit: "seconds", Min: 1, Max: 60},
	"policy_refresh_seconds":           {Type: "integer", Unit: "seconds", Min: 1, Max: 300},
}

func configSettingValues(c Config) map[string]int64 {
	return map[string]int64{
		"identity_rotation_window_seconds": int64(c.IdentityRotationWindow / time.Second), "frames_per_minute": int64(c.FramesPerMinute), "bytes_per_minute": int64(c.BytesPerMinute), "max_connections": int64(c.MaxConnections), "max_streams": int64(c.MaxStreams), "stream_bytes_per_second": int64(c.StreamBytesPerSecond), "stream_wait_seconds": int64(c.StreamWait / time.Second), "stream_idle_seconds": int64(c.StreamIdle / time.Second), "connection_idle_seconds": int64(c.ConnectionIdle / time.Second), "hello_timeout_seconds": int64(c.HelloTimeout / time.Second), "policy_refresh_seconds": int64(c.PolicyRefresh / time.Second),
	}
}
func (s *Store) Settings() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT key,value FROM hub_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var key string
		var val int64
		if err = rows.Scan(&key, &val); err != nil {
			return nil, err
		}
		spec, ok := settingSpecs[key]
		if !ok || val < spec.Min || val > spec.Max {
			return nil, fmt.Errorf("invalid persisted hub setting %q", key)
		}
		out[key] = val
	}
	return out, rows.Err()
}
func (s *Store) PatchSettings(overrides map[string]*int64) error {
	if len(overrides) == 0 {
		return adminErr(400, "settings", "select at least one setting")
	}
	for key, value := range overrides {
		spec, ok := settingSpecs[key]
		if !ok {
			return adminErr(400, "settings", "unknown or host-only setting: "+key)
		}
		if value != nil && (*value < spec.Min || *value > spec.Max) {
			return adminErr(400, "settings", fmt.Sprintf("%s must be %d..%d", key, spec.Min, spec.Max))
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for key, value := range overrides {
		if value == nil {
			_, err = tx.Exec(`DELETE FROM hub_settings WHERE key=?`, key)
		} else {
			_, err = tx.Exec(`INSERT INTO hub_settings VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, *value)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
func effectiveConfig(c Config, overrides map[string]int64) Config {
	for key, value := range overrides {
		d := time.Duration(value) * time.Second
		switch key {
		case "identity_rotation_window_seconds":
			c.IdentityRotationWindow = d
		case "frames_per_minute":
			c.FramesPerMinute = int(value)
		case "bytes_per_minute":
			c.BytesPerMinute = int(value)
		case "max_connections":
			c.MaxConnections = int(value)
		case "max_streams":
			c.MaxStreams = int(value)
		case "stream_bytes_per_second":
			c.StreamBytesPerSecond = int(value)
		case "stream_wait_seconds":
			c.StreamWait = d
		case "stream_idle_seconds":
			c.StreamIdle = d
		case "connection_idle_seconds":
			c.ConnectionIdle = d
		case "hello_timeout_seconds":
			c.HelloTimeout = d
		case "policy_refresh_seconds":
			c.PolicyRefresh = d
		}
	}
	return c
}
func (h *Hub) config() Config {
	if c := h.effective.Load(); c != nil {
		return *c
	}
	return h.cfg
}
func (h *Hub) Settings() (map[string]Setting, error) {
	overrides, err := h.store.Settings()
	if err != nil {
		return nil, err
	}
	boot, effective := configSettingValues(h.cfg), configSettingValues(effectiveConfig(h.cfg, overrides))
	out := map[string]Setting{}
	for key, spec := range settingSpecs {
		spec.Boot = boot[key]
		spec.Effective = effective[key]
		spec.Source = "default"
		if slices.Contains(h.cfg.FlagSettings, key) {
			spec.Source = "flag"
		}
		if _, ok := overrides[key]; ok {
			spec.Source = "db"
			spec.FlagOverridden = slices.Contains(h.cfg.FlagSettings, key)
		}
		out[key] = spec
	}
	return out, nil
}
func (h *Hub) refreshSettings() error {
	h.settingsMu.Lock()
	defer h.settingsMu.Unlock()
	overrides, err := h.store.Settings()
	if err != nil {
		return err
	}
	c := effectiveConfig(h.cfg, overrides)
	h.effective.Store(&c)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, lim := range h.limiters {
		lim.mu.Lock()
		lim.frames.capacity = float64(c.FramesPerMinute)
		lim.frames.tokens = min(lim.frames.tokens, lim.frames.capacity)
		lim.bytes.capacity = float64(c.BytesPerMinute)
		lim.bytes.tokens = min(lim.bytes.tokens, lim.bytes.capacity)
		lim.mu.Unlock()
	}
	if h.streams != nil {
		for _, lim := range h.streams.limiters {
			lim.mu.Lock()
			lim.rate = float64(c.StreamBytesPerSecond)
			lim.tokens = min(lim.tokens, lim.rate)
			lim.mu.Unlock()
		}
	}
	return nil
}
