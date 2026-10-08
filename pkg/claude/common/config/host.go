package config

import "math"

// HostConfig tunes local resource warnings. Zero disables a warning; omitted
// thresholds use defaults. WorkDirs adds roots beyond home and active groups'
// default directories. No directory is created by the sampler.
type HostConfig struct {
	WorkDirs                 []string `json:"work_dirs,omitempty"`
	WarnLoadPerCore          *float64 `json:"warn_load_per_core,omitempty"`
	WarnRAMAvailablePercent  *float64 `json:"warn_ram_available_percent,omitempty"`
	WarnDiskAvailablePercent *float64 `json:"warn_disk_available_percent,omitempty"`
}

// HostWarningThresholds resolves warning defaults, with a safe fallback for
// invalid values in hand-edited config files.
func (c *Config) HostWarningThresholds() (load, ram, disk float64) {
	load, ram, disk = 1.5, 10, 10
	if c == nil || c.Host == nil {
		return
	}
	resolve := func(value *float64, fallback, max float64) float64 {
		if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || (max > 0 && *value > max) {
			return fallback
		}
		return *value
	}
	load = resolve(c.Host.WarnLoadPerCore, load, 0)
	ram = resolve(c.Host.WarnRAMAvailablePercent, ram, 100)
	disk = resolve(c.Host.WarnDiskAvailablePercent, disk, 100)
	return
}
