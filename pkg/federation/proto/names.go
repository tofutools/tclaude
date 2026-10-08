package proto

import (
	"strings"
	"time"
)

// MaxNameLen bounds every remote-supplied display name.
const MaxNameLen = 64

// SafeName reduces a remote-supplied display name (instance name, agent
// name, group name) to a conservative charset before it is stored or shown.
// These strings end up in tmux nudges and inbox headers, which are
// injection sinks: anything outside [A-Za-z0-9 ._+-] (and '@' when allowAt)
// becomes '_', runs of whitespace collapse, and the result is capped at
// MaxNameLen. An empty result becomes "unknown".
func SafeName(s string, allowAt bool) string {
	var b strings.Builder
	n := 0
	lastSpace := false
	for _, r := range s {
		if n >= MaxNameLen {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-', r == '+':
			b.WriteRune(r)
			lastSpace = false
		case r == '@' && allowAt:
			b.WriteRune(r)
			lastSpace = false
		case r == ' ' || r == '\t':
			if lastSpace || b.Len() == 0 {
				continue
			}
			b.WriteRune(' ')
			lastSpace = true
		default:
			b.WriteRune('_')
			lastSpace = false
		}
		n++
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "unknown"
	}
	return out
}

// ValidAgentRef reports whether s looks like an agent id (agt_ + [0-9a-z]).
func ValidAgentRef(s string) bool {
	if !strings.HasPrefix(s, "agt_") || len(s) > MaxNameLen || len(s) < 5 {
		return false
	}
	for _, r := range s[4:] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// SanitizeCatalog applies SafeName to every name in a received catalog and
// drops members whose agent ref is malformed.
func SanitizeCatalog(c *CatalogPayload) {
	for gi := range c.Groups {
		g := &c.Groups[gi]
		g.Name = SafeName(g.Name, false)
		g.Description = safeOptional(g.Description)
		var caps []string
		for _, cp := range g.Caps {
			for _, known := range AllCaps {
				if cp == known {
					caps = append(caps, cp)
				}
			}
		}
		g.Caps = caps
		members := g.Members[:0]
		for _, m := range g.Members {
			if !ValidAgentRef(m.Agent) {
				continue
			}
			m.Name = SafeName(m.Name, false)
			m.Role = safeOptional(m.Role)
			if m.Presence != "" && m.Presence != "online" && m.Presence != "offline" {
				m.Presence = ""
			}
			members = append(members, m)
		}
		g.Members = members
		g.Sessions = SanitizeSessions(g.Sessions)
		if !g.HasCap(CapSessions) {
			g.Sessions = nil
			g.SessionsAt = time.Time{}
		}
		routes := g.Routes[:0]
		for _, rt := range g.Routes {
			if !ValidRouteID(rt.ID) {
				continue
			}
			rt.Publisher = SafeName(rt.Publisher, false)
			rt.Name = SafeName(rt.Name, false)
			routes = append(routes, rt)
		}
		g.Routes = routes
	}
}

// ValidRouteID reports whether s has the shape of a route id ("rte_" + hex).
func ValidRouteID(s string) bool {
	if !strings.HasPrefix(s, "rte_") || len(s) < 8 || len(s) > 64 {
		return false
	}
	for _, r := range s[4:] {
		if (r < 'a' || r > 'f') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func safeOptional(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return SafeName(s, false)
}

// StripControls removes C0/C1 control characters (and DEL) from remote
// text, keeping newlines and tabs, so it cannot drive a terminal.
func StripControls(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return -1
		}
		return r
	}, s)
}

// SanitizeSessions validates identities and bounds remote display text.
func SanitizeSessions(in []CatalogSession) []CatalogSession {
	out := in[:0]
	for _, s := range in {
		if !ValidAgentRef(s.Agent) || s.Session == "" || len(s.Session) > 128 {
			continue
		}
		valid := true
		for _, r := range s.Session {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
				valid = false
			}
		}
		if !valid {
			continue
		}
		s.Name = SafeName(s.Name, false)
		s.Harness = safeOptional(s.Harness)
		s.State = SafeName(s.State, false)
		switch s.WaitingReason {
		case "permission", "question", "prompt":
		default:
			s.WaitingReason = ""
			s.WaitingObservedSince = nil
		}
		out = append(out, s)
	}
	return out
}
