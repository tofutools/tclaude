package config

import (
	"slices"
)

// NotificationConfig holds settings for OS notifications.
type NotificationConfig struct {
	Enabled             bool             `json:"enabled"`
	Transitions         []TransitionRule `json:"transitions,omitempty"`
	CooldownSeconds     int              `json:"cooldown_seconds,omitempty"`
	NotificationCommand []string         `json:"notification_command,omitempty"`

	// HumanMessages controls whether a `tclaude agent notify-human`
	// message also raises an OS notification (the desktop companion to
	// the dashboard Messages tab). It is a *bool so the unset/zero state
	// is distinguishable from an explicit false: within an enabled
	// notification block it defaults ON — the human asked notify-human to
	// also ping the desktop — and is silenced only by an explicit
	// "human_messages": false. See NotifyHumanMessages.
	HumanMessages *bool `json:"human_messages,omitempty"`

	// Delivery selects WHERE an already-decided notification is raised.
	// Every gate above it (enabled, transitions, per-agent/group filters,
	// cooldown) is unchanged — this only picks the output channel:
	//
	//	"os"      — the platform notifier (D-Bus / toast / terminal-notifier),
	//	            or notification_command when set. The historical
	//	            behaviour and the default for an empty value.
	//	"browser" — the notification is queued for the agentd dashboard,
	//	            which raises a Web Notification from the browser. Works
	//	            when the human is remote (the dashboard is reachable but
	//	            the daemon's desktop is not) and when the notifying
	//	            process is sandboxed away from the session D-Bus.
	//	"both"    — raise it in both places.
	//
	// Browser delivery needs a dashboard tab open, granted the browser's
	// notification permission, in a secure context (https or localhost).
	Delivery string `json:"delivery,omitempty"`

	// KnownTypes records the per-type categories (NotifyTypes) this block
	// has already been offered, so `tclaude setup` can tell a category the
	// human switched off (known, rule absent → leave it off) from one
	// introduced by a newer tclaude (unknown → add it). Empty means a block
	// written before tracking existed; it is treated as knowing
	// notifyTypesBaseline. See MergeDefaultTypes.
	KnownTypes []string `json:"known_types,omitempty"`
}

// Notification delivery channels — the accepted values of
// NotificationConfig.Delivery. NotifyDeliveryOS is also the meaning of
// the empty/unset value.
const (
	NotifyDeliveryOS      = "os"
	NotifyDeliveryBrowser = "browser"
	NotifyDeliveryBoth    = "both"
)

// IsNotifyDelivery reports whether s is an accepted delivery value. The
// empty string is accepted — it is the unset state, which reads as "os".
func IsNotifyDelivery(s string) bool {
	switch s {
	case "", NotifyDeliveryOS, NotifyDeliveryBrowser, NotifyDeliveryBoth:
		return true
	}
	return false
}

// DeliverToOS reports whether notifications should reach the platform
// notifier. Unset/unknown → true, so a config written by a newer tclaude
// (or a typo that slipped past Validate) degrades to the historical
// desktop behaviour rather than to silence.
func (c *NotificationConfig) DeliverToOS() bool {
	if c == nil {
		return true
	}
	return c.Delivery != NotifyDeliveryBrowser
}

// DeliverToBrowser reports whether notifications should also be queued
// for the dashboard to raise as Web Notifications.
func (c *NotificationConfig) DeliverToBrowser() bool {
	if c == nil {
		return false
	}
	return c.Delivery == NotifyDeliveryBrowser || c.Delivery == NotifyDeliveryBoth
}

// NotifyHumanMessages reports whether a notify-human message should also
// raise an OS notification. It requires the master switch (Enabled);
// within that it defaults ON and is suppressed only by an explicit
// "human_messages": false. nil receiver / disabled block → false.
func (c *NotificationConfig) NotifyHumanMessages() bool {
	if c == nil || !c.Enabled {
		return false
	}
	return c.HumanMessages == nil || *c.HumanMessages
}

// RateLimitConfig holds settings for rate limit
type RateLimitConfig struct {
	FiveHourPercentMaxUsed float64 `json:"five_hour_percent_max_used"`
	SevenDayPercentMaxUsed float64 `json:"seven_day_percent_max_used"`
}

// TransitionRule defines a state transition that triggers a notification.
// Use "*" as a wildcard to match any state. A self-transition (from ==
// to, e.g. an idle session re-stamped idle) never notifies, regardless
// of rules — notify.OnStateTransition drops it before matching.
type TransitionRule struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// MatchesTransition checks if a state transition matches any configured rule.
func (c *NotificationConfig) MatchesTransition(from, to string) bool {
	if c == nil || !c.Enabled {
		return false
	}

	for _, rule := range c.Transitions {
		fromMatch := rule.From == "*" || rule.From == from
		toMatch := rule.To == "*" || rule.To == to
		if fromMatch && toMatch {
			return true
		}
	}
	return false
}

// NotifyTypes is the canonical set of destination states the friendly
// per-type notification selector (the top-bar bell popover and the Config
// tab checklist) toggles. Each "type" the human checks/unchecks maps to a
// wildcard transition rule {from:"*", to:<state>} — so the selector is a
// human-readable view over the lower-level Transitions list, not a second
// storage model. The order here is the order the UI renders. It mirrors
// the default DefaultConfig().Notifications.Transitions destinations.
var NotifyTypes = []string{
	"idle",
	"awaiting_permission",
	"awaiting_input",
	"error",
	"exited",
}

// notifyTypesBaseline is the NotifyTypes set that existed before
// NotificationConfig.KnownTypes was tracked. A block without KnownTypes is
// treated as already knowing these, so a category the human unchecked is
// not re-enabled by setup. Frozen: append new categories to NotifyTypes
// only, never here.
var notifyTypesBaseline = []string{
	"idle",
	"awaiting_permission",
	"awaiting_input",
	"error",
	"exited",
}

// IsNotifyType reports whether to is one of the canonical NotifyTypes the
// per-type selector manages. Transitions to any other state (or with a
// non-wildcard From) are "advanced" rules the selector leaves untouched.
func IsNotifyType(to string) bool {
	return slices.Contains(NotifyTypes, to)
}

// NotifyTypeEnabled reports whether the friendly per-type checkbox for the
// destination state `to` is on — i.e. whether a wildcard rule {from:"*",
// to:to} is present in Transitions. A from-specific rule (e.g.
// {from:"working", to:"idle"}) is an advanced rule and does NOT light the
// checkbox; it is preserved untouched by SetNotifyType.
func (c *NotificationConfig) NotifyTypeEnabled(to string) bool {
	if c == nil {
		return false
	}
	for _, r := range c.Transitions {
		if r.From == "*" && r.To == to {
			return true
		}
	}
	return false
}

// SetNotifyType turns the friendly per-type notification on/off for the
// destination state `to` by adding or removing the single wildcard rule
// {from:"*", to:to}. Every other rule — from-specific rules and rules to
// non-canonical destinations — round-trips untouched, so the checklist and
// the raw "Advanced" transitions editor never clobber each other. on=true
// is idempotent (a duplicate wildcard rule is never added); on=false drops
// every wildcard rule for that destination.
func (c *NotificationConfig) SetNotifyType(to string, on bool) {
	if c == nil {
		return
	}
	// Rebuild without any wildcard rule for this destination; fresh
	// backing array (cap 0) so we never mutate a slice the caller may
	// still be aliasing.
	kept := make([]TransitionRule, 0, len(c.Transitions)+1)
	for _, r := range c.Transitions {
		if r.From == "*" && r.To == to {
			continue
		}
		kept = append(kept, r)
	}
	if on {
		kept = append(kept, TransitionRule{From: "*", To: to})
	}
	c.Transitions = kept
}

// MergeDefaultTypes additively enables every currently-supported
// notification category (NotifyTypes) the block has not been offered
// before (absent from KnownTypes, or from notifyTypesBaseline when
// KnownTypes is empty), returning the destinations it added (in NotifyTypes
// order; nil if none), and then marks every category known. A category the
// human already knew about and switched off stays off. Existing rules —
// including from-specific "advanced" rules — and the
// cooldown/command/human-message settings are left untouched. This is how
// `tclaude setup` picks up categories introduced in a newer tclaude
// version without overwriting the user's other notification choices. A
// nil receiver returns nil.
func (c *NotificationConfig) MergeDefaultTypes() []string {
	if c == nil {
		return nil
	}
	known := c.knownNotifyTypes()
	var added []string
	for _, ty := range NotifyTypes {
		if !slices.Contains(known, ty) && !c.NotifyTypeEnabled(ty) {
			c.SetNotifyType(ty, true)
			added = append(added, ty)
		}
	}
	c.MarkNotifyTypesKnown()
	return added
}

// MarkNotifyTypesKnown records every current NotifyTypes category as
// offered, so a later MergeDefaultTypes respects their on/off state. Call
// it wherever the human edits the per-type selection. Categories already
// known but absent from this binary's NotifyTypes (written by a newer
// tclaude) are kept, so a downgrade-then-upgrade cannot re-enable them. It
// writes KnownTypes only when the set grows, so a block whose baseline
// already covers NotifyTypes stays minimal on disk.
func (c *NotificationConfig) MarkNotifyTypesKnown() {
	if c == nil {
		return
	}
	known := c.knownNotifyTypes()
	merged := slices.Clone(known)
	for _, ty := range NotifyTypes {
		if !slices.Contains(merged, ty) {
			merged = append(merged, ty)
		}
	}
	if len(merged) != len(known) {
		c.KnownTypes = merged
	}
}

// knownNotifyTypes is KnownTypes, or notifyTypesBaseline for a block that
// predates tracking.
func (c *NotificationConfig) knownNotifyTypes() []string {
	if len(c.KnownTypes) == 0 {
		return notifyTypesBaseline
	}
	return c.KnownTypes
}

// HumanMessagesIntent reports the human-messages preference independent of
// the master Enabled switch: it is the value the per-type "Sends me a
// message" checkbox should show. Unset (nil) defaults ON, matching
// NotifyHumanMessages's within-enabled default; only an explicit false is
// off. Distinct from NotifyHumanMessages, which additionally ANDs Enabled
// (the effective "should this banner fire" decision).
func (c *NotificationConfig) HumanMessagesIntent() bool {
	if c == nil {
		return true
	}
	return c.HumanMessages == nil || *c.HumanMessages
}
