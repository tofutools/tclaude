package config

import "slices"

// PushConfig is Web Push delivery: notifications sent to the operator's
// subscribed devices (a phone's home-screen app, a desktop browser) through
// their push service, so they arrive with the app closed.
//
// It is a channel of its own beside Delivery (the desktop's os/browser
// choice), with its own on/off per category. Two gates are shared with the
// desktop: the master switch (NotificationConfig.Enabled) and the
// per-agent / per-group mute filters. Nothing is sent until a device
// subscribes: the categories say what a subscribed device gets.
//
// It is the node operator's own: config_classes.go classes the block local,
// so no peer changes it, and no agent path writes it.
type PushConfig struct {
	// Categories turns each PushCategories entry on or off. A category
	// absent from the map has its default (PushCategoryDefault).
	Categories map[string]bool `json:"categories,omitempty"`
	// ContentFree sends pushes without any text: the device shows
	// "tclaude needs you", and the app shows what it is once opened. The
	// push service then learns nothing but that something happened.
	ContentFree bool `json:"content_free,omitempty"`
}

// Push categories beyond the per-state NotifyTypes.
const (
	PushAccessRequests = "access_requests"
	PushHumanMessages  = "human_messages"
	PushPresentPR      = "present_pr"
	// PushOperatorQuestions: a question waiting for the operator's answer
	// (a taskboard ask-operator question, a notify-human decision card, a
	// process step waiting on a person).
	PushOperatorQuestions = "operator_questions"
)

// PushCategories is every category a push can be about, in display order:
// the --ask-human access requests, questions for the operator, notify-human
// messages, presented PRs, then an agent's state changes (NotifyTypes).
var PushCategories = append([]string{PushAccessRequests, PushOperatorQuestions, PushHumanMessages, PushPresentPR}, NotifyTypes...)

// pushDefaultOn is what a newly subscribed device gets: an agent stuck
// waiting on the operator, for a permission or an answer. Everything else
// is off until asked for: an idle or exited agent, an error, a presented
// PR, and a plain notify-human message, which is often a status update.
var pushDefaultOn = []string{PushAccessRequests, PushOperatorQuestions, "awaiting_permission", "awaiting_input"}

// IsPushCategory reports whether s is one of PushCategories.
func IsPushCategory(s string) bool { return slices.Contains(PushCategories, s) }

// PushCategoryDefault is a category's state when the config does not set it.
func PushCategoryDefault(category string) bool { return slices.Contains(pushDefaultOn, category) }

// PushCategoryOn reports whether the category is on for push, before the
// master switch: the per-category toggle, or its default.
func (c *NotificationConfig) PushCategoryOn(category string) bool {
	if !IsPushCategory(category) {
		return false
	}
	if c != nil && c.Push != nil {
		if on, ok := c.Push.Categories[category]; ok {
			return on
		}
	}
	return PushCategoryDefault(category)
}

// PushWanted reports whether a notification in category should go to the
// subscribed devices: the master switch and the category. The caller still
// applies the mute filters; with no device subscribed nothing is sent.
func (c *NotificationConfig) PushWanted(category string) bool {
	return c != nil && c.Enabled && c.PushCategoryOn(category)
}

// PushContentFree reports whether pushes carry no text.
func (c *NotificationConfig) PushContentFree() bool {
	return c != nil && c.Push != nil && c.Push.ContentFree
}

// SetPushCategory stores a category's toggle; one set to its default is
// dropped, so the block only holds choices.
func (c *NotificationConfig) SetPushCategory(category string, on bool) {
	if c.Push == nil {
		c.Push = &PushConfig{}
	}
	if on == PushCategoryDefault(category) {
		delete(c.Push.Categories, category)
	} else {
		if c.Push.Categories == nil {
			c.Push.Categories = map[string]bool{}
		}
		c.Push.Categories[category] = on
	}
	if len(c.Push.Categories) == 0 {
		c.Push.Categories = nil
	}
	if c.Push.Categories == nil && !c.Push.ContentFree {
		c.Push = nil
	}
}

// SetPushContentFree stores the content-free mode.
func (c *NotificationConfig) SetPushContentFree(on bool) {
	if c.Push == nil {
		if !on {
			return
		}
		c.Push = &PushConfig{}
	}
	c.Push.ContentFree = on
	if c.Push.Categories == nil && !c.Push.ContentFree {
		c.Push = nil
	}
}

func validatePush(p *PushConfig) []string {
	if p == nil {
		return nil
	}
	var errs []string
	for k := range p.Categories {
		if !IsPushCategory(k) {
			errs = append(errs, "notifications.push.categories has an unknown category "+`"`+k+`"`)
		}
	}
	slices.Sort(errs)
	return errs
}
