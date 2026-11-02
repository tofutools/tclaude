package config

// CapacityConfig limits how many agents this instance runs at once and what
// free RAM and disk a new launch needs. Everything zero or omitted means no
// limit, so an instance without the block behaves as before.
type CapacityConfig struct {
	// MaxLiveAgents caps live and reserved managed agents; zero is unlimited.
	// When zero, the legacy federation.max_live_agents still applies.
	MaxLiveAgents int `json:"max_live_agents,omitempty"`
	// MinFreeRAMMB and MinFreeDiskMB are the free memory and the free space on
	// the data directory's and the launch directory's filesystems a new launch
	// needs; zero disables the check.
	MinFreeRAMMB  int64 `json:"min_free_ram_mb,omitempty"`
	MinFreeDiskMB int64 `json:"min_free_disk_mb,omitempty"`
	// WhenFull is "refuse" (the default) or "queue".
	WhenFull string `json:"when_full,omitempty"`
	// MaxQueued caps waiting spawns in queue mode; zero means the default.
	MaxQueued int `json:"max_queued,omitempty"`
}

const (
	CapacityWhenFullRefuse  = "refuse"
	CapacityWhenFullQueue   = "queue"
	DefaultCapacityMaxQueue = 50
)

// CapacityLimits is the resolved capacity policy.
type CapacityLimits struct {
	MaxLiveAgents int
	MinFreeRAMMB  int64
	MinFreeDiskMB int64
	Queue         bool
	MaxQueued     int
}

// Limited reports whether any limit is configured.
func (l CapacityLimits) Limited() bool {
	return l.MaxLiveAgents > 0 || l.MinFreeRAMMB > 0 || l.MinFreeDiskMB > 0
}

// ResolvedCapacity returns the effective policy. capacity.max_live_agents
// wins; federation.max_live_agents is the older spelling of the same cap.
// Invalid hand-edited values fall back to no limit rather than a surprise.
func (c *Config) ResolvedCapacity() CapacityLimits {
	out := CapacityLimits{MaxQueued: DefaultCapacityMaxQueue}
	if c == nil {
		return out
	}
	if c.Federation != nil && c.Federation.MaxLiveAgents > 0 {
		out.MaxLiveAgents = c.Federation.MaxLiveAgents
	}
	k := c.Capacity
	if k == nil {
		return out
	}
	if k.MaxLiveAgents > 0 {
		out.MaxLiveAgents = k.MaxLiveAgents
	}
	out.MinFreeRAMMB = max(k.MinFreeRAMMB, 0)
	out.MinFreeDiskMB = max(k.MinFreeDiskMB, 0)
	out.Queue = k.WhenFull == CapacityWhenFullQueue
	if k.MaxQueued > 0 {
		out.MaxQueued = k.MaxQueued
	}
	return out
}

func validateCapacity(k *CapacityConfig) []string {
	if k == nil {
		return nil
	}
	var errs []string
	if k.MaxLiveAgents < 0 {
		errs = append(errs, "capacity.max_live_agents must be nonnegative (zero is unlimited)")
	}
	if k.MinFreeRAMMB < 0 || k.MinFreeDiskMB < 0 {
		errs = append(errs, "capacity.min_free_ram_mb and capacity.min_free_disk_mb must be nonnegative (zero is off)")
	}
	switch k.WhenFull {
	case "", CapacityWhenFullRefuse, CapacityWhenFullQueue:
	default:
		errs = append(errs, `capacity.when_full must be "refuse" or "queue"`)
	}
	if k.MaxQueued < 0 {
		errs = append(errs, "capacity.max_queued must be nonnegative (zero means the default, 50)")
	}
	return errs
}
