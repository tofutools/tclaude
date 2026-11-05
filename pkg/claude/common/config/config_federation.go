package config

import (
	"fmt"
	"math"
	"time"
)

// FederationConfig configures the outbound hub connection.
// FederationAwayConfig is the operator's selected covering instance. Live
// approval authority is deliberately not persisted across daemon restarts.
type FederationAwayConfig struct {
	Cover string    `json:"cover"`
	Since time.Time `json:"since"`
	Until time.Time `json:"until,omitempty"`
}

// FederationHealthPolicy controls operator notices; resource/failure signals are opt-in.
type FederationHealthPolicy struct {
	Presence             *bool   `json:"presence,omitempty"`
	Resources            bool    `json:"resources"`
	Failures             bool    `json:"failures"`
	DebounceSeconds      int     `json:"debounce_seconds,omitempty"`
	DiskFreePercent      float64 `json:"disk_free_percent,omitempty"`
	RAMFreePercent       float64 `json:"ram_free_percent,omitempty"`
	MemorySeconds        int     `json:"memory_seconds,omitempty"`
	FailureCount         int     `json:"failure_count,omitempty"`
	FailureWindowSeconds int     `json:"failure_window_seconds,omitempty"`
	CooldownSeconds      int     `json:"cooldown_seconds,omitempty"`
}

func (p FederationHealthPolicy) Validate() error {
	for _, n := range []int{p.DebounceSeconds, p.MemorySeconds, p.FailureWindowSeconds, p.CooldownSeconds} {
		if n < 0 || n > 86400 {
			return fmt.Errorf("durations must be 0..86400 seconds (zero uses default)")
		}
	}
	if p.FailureCount < 0 || p.FailureCount > 256 {
		return fmt.Errorf("failure_count must be 0..256")
	}
	for _, n := range []float64{p.DiskFreePercent, p.RAMFreePercent} {
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 100 {
			return fmt.Errorf("free percentages must be 0..100")
		}
	}
	return nil
}

type FederationHealthConfig struct {
	Defaults FederationHealthPolicy `json:"defaults,omitempty"`
	// Keys are immutable instance IDs. Accepted linked successors inherit an
	// ancestor's settings unless they have an explicit override.
	Peers map[string]FederationHealthPolicy `json:"peers,omitempty"`
}

type NodeScriptsConfig struct {
	AcceptRemoteScripts bool     `json:"accept_remote_scripts"`
	Memory              string   `json:"memory,omitempty"`
	CPU                 *float64 `json:"cpu,omitempty"`
	PIDs                *uint64  `json:"pids,omitempty"`
}

type FederationConfig struct {
	// BoardUpdateNotify leaves a Messages note when a kept content-board item
	// gets a newer version. Off by default; the dashboard badge works regardless.
	BoardUpdateNotify bool `json:"board_update_notify,omitempty"`
	// AgentTransferMaxBytes caps agent archives and uncompressed history; zero defaults to 2 GiB.
	AgentTransferMaxBytes int64 `json:"agent_transfer_max_bytes,omitempty"`
	// AgentHistoryRecordMaxBytes is the JSONL record memory bound; zero defaults to256 MiB.
	AgentHistoryRecordMaxBytes int                     `json:"agent_history_record_max_bytes,omitempty"`
	Scripts                    *NodeScriptsConfig      `json:"scripts,omitempty"`
	Health                     *FederationHealthConfig `json:"health,omitempty"`

	// IdentityRotationSeconds is the local successor detection window; default 600.
	IdentityRotationSeconds int                       `json:"identity_rotation_seconds,omitempty"`
	Teleport                *FederationTeleportConfig `json:"teleport,omitempty"`
	// NodeLabels is a local set, shared by operator and future node-profile writers.
	NodeLabels []string `json:"node_labels,omitempty"`
	// MaxLiveAgents advertises node capacity and the receiver enforces it on
	// incoming work; zero is unlimited.
	MaxLiveAgents int                   `json:"max_live_agents,omitempty"`
	Away          *FederationAwayConfig `json:"away,omitempty"`
	// UnrestrictedMaxLive caps automatic workers per unrestricted peer. Default 8.
	UnrestrictedMaxLive int `json:"unrestricted_max_live,omitempty"`
	// Enabled starts the hub client. Default false.
	Enabled bool `json:"enabled,omitempty"`
	// HubURL is the hub's WebSocket URL: wss://host[:port], or ws:// to a
	// loopback hub only.
	HubURL string `json:"hub_url,omitempty"`
	// Name is this instance's display name on the hub (default
	// user@hostname).
	Name string `json:"name,omitempty"`
	// Invite is a single-use hub invite token, presented until admitted.
	Invite string `json:"invite,omitempty"`
	// HubCAFile is an optional PEM bundle trusted for the hub's TLS
	// certificate instead of the system roots.
	HubCAFile string `json:"hub_ca_file,omitempty"`
}

// FederationTeleportConfig is local policy. Freeze every instance to freeze a
// disconnected fleet; this switch does not claim distributed consensus.
type FederationTeleportConfig struct {
	Disabled bool                 `json:"disabled,omitempty"`
	Limits   TeleportLimits       `json:"limits,omitempty"`
	Backup   TeleportBackupConfig `json:"backup,omitempty"`
}
type TeleportLimits struct {
	Hour           int  `json:"per_hour,omitempty"`
	Day            int  `json:"per_day,omitempty"`
	Chain          int  `json:"per_chain,omitempty"`
	RevisitMinutes int  `json:"revisit_minutes,omitempty"`
	AllowReturn    bool `json:"allow_return,omitempty"`
}

func (l TeleportLimits) Effective() TeleportLimits {
	if l.Hour == 0 {
		l.Hour = 4
	}
	if l.Day == 0 {
		l.Day = 12
	}
	if l.Chain == 0 {
		l.Chain = 16
	}
	if l.RevisitMinutes == 0 {
		l.RevisitMinutes = 10
	}
	return l
}

// ModelProxyPolicy explicitly opts a named HTTP service into an Anthropic
// Messages or OpenAI Responses gateway. Zero budgets never mean unlimited: all six daily limits
// and both token bounds must be positive. Incomplete requests retain their
// conservative input+output reservation until the UTC day rolls over.
type ModelProxyPolicy struct {
	Dialect              string   `json:"dialect,omitempty"`
	LeaseIdleHours       int      `json:"lease_idle_hours,omitempty"`
	PrecountInput        bool     `json:"precount_input,omitempty"`
	BlockedPeers         []string `json:"blocked_peers,omitempty"`
	Enabled              bool     `json:"enabled"`
	Models               []string `json:"models"`
	DailyRequests        int64    `json:"daily_requests"`
	DailyTokens          int64    `json:"daily_tokens"`
	PeerDailyRequests    int64    `json:"peer_daily_requests"`
	PeerDailyTokens      int64    `json:"peer_daily_tokens"`
	SessionDailyRequests int64    `json:"session_daily_requests"`
	SessionDailyTokens   int64    `json:"session_daily_tokens"`
	MaxInputTokens       int64    `json:"max_input_tokens"`
	MaxOutputTokens      int64    `json:"max_output_tokens"`
	MaxConcurrent        int      `json:"max_concurrent"`
	RequestsPerMinute    int      `json:"requests_per_minute"`
	// Byte bounds are independent safety limits, never substitutes for tokens.
	MaxRequestBytes    int64 `json:"max_request_bytes,omitempty"`
	MaxResponseBytes   int64 `json:"max_response_bytes,omitempty"`
	MaxEventBytes      int   `json:"max_event_bytes,omitempty"`
	MaxDurationSeconds int   `json:"max_duration_seconds,omitempty"`
	// Prices adds or overrides the gateway's model prices (USD per million
	// tokens), keyed by model ID or a trailing-* prefix. Usage sharing
	// prices requests with them; built-in Claude and OpenAI API rates apply
	// otherwise.
	Prices map[string]ModelProxyPrice `json:"prices,omitempty"`
}

// ModelProxyPrice is one model's rates in USD per million tokens. A zero
// cache rate falls back to the input rate.
type ModelProxyPrice struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read,omitempty"`
	CacheWrite   float64 `json:"cache_write,omitempty"`
	CacheWrite1h float64 `json:"cache_write_1h,omitempty"`
}

// TeleportBackupConfig deliberately prefers remote availability during an
// origin outage. Epoch reconciliation resolves overlap after a partition.
type TeleportBackupConfig struct {
	RenewSeconds int    `json:"renew_seconds,omitempty"`
	LeaseSeconds int    `json:"lease_seconds,omitempty"`
	GraceSeconds int    `json:"grace_seconds,omitempty"`
	DormantMax   int    `json:"dormant_max,omitempty"`
	Recovery     string `json:"recovery,omitempty"`
	Superseded   string `json:"superseded,omitempty"`
}

func (c TeleportBackupConfig) Effective() TeleportBackupConfig {
	if c.RenewSeconds == 0 {
		c.RenewSeconds = 30
	}
	if c.LeaseSeconds == 0 {
		c.LeaseSeconds = 300
	}
	if c.GraceSeconds == 0 {
		c.GraceSeconds = 120
	}
	if c.DormantMax == 0 {
		c.DormantMax = 4
	}
	if c.Recovery == "" {
		c.Recovery = "auto"
	}
	if c.Superseded == "" {
		c.Superseded = "stop"
	}
	return c
}
func (c TeleportBackupConfig) Validate() error {
	c = c.Effective()
	if c.RenewSeconds < 1 || c.RenewSeconds > 43200 || c.LeaseSeconds < c.RenewSeconds*2 || c.LeaseSeconds > 86400 || c.GraceSeconds < 1 || c.GraceSeconds > 86400 || c.DormantMax < 1 || c.DormantMax > 10000 || c.Recovery != "auto" && c.Recovery != "manual" || c.Superseded != "stop" && c.Superseded != "clone" {
		return fmt.Errorf("invalid teleport backup policy")
	}
	return nil
}
