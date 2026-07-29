// Package standingorders evaluates trigger-driven standing orders and reports,
// honestly, what each harness can actually do with them.
//
// The package is deliberately split in two halves that do not know about each
// other's problems:
//
//   - capability.go answers "what timing can this harness give this trigger",
//     a pure function of (timing, event, harness) with no IO at all;
//   - evaluate.go answers "does this order fire for this event, and what
//     happened", given a capability answer and a cadence lookup.
//
// Neither half delivers anything. Delivery is the caller's job, because the
// two transports live in different layers (hook stdout inside the agent's
// pane, messages inside agentd) and pulling either of them in here would make
// the evaluator untestable without a running daemon.
package standingorders

import (
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Status is how well a harness can honour an order's required timing.
type Status string

const (
	// StatusSupported — the harness can deliver at the required timing.
	StatusSupported Status = "supported"
	// StatusDegraded — the harness will deliver, but on a weaker transport
	// than the order asked for. Only ever reported for an order that asked
	// for next-turn timing, because next-turn is the weakest guarantee there
	// is; an order requiring same-continuation is never quietly downgraded.
	StatusDegraded Status = "degraded"
	// StatusUnsupported — the harness cannot meet the required timing, and
	// nothing will be delivered.
	StatusUnsupported Status = "unsupported"
)

// Capability is the answer for one (timing, event, harness) triple. It is
// derived, never stored: storing it would let a stale row outlive a harness
// gaining or losing a channel.
type Capability struct {
	Status    Status `json:"status"`
	Transport string `json:"transport"`
	Detail    string `json:"detail,omitempty"`
}

// Supported reports whether anything will be delivered at all.
func (c Capability) Supported() bool { return c.Status != StatusUnsupported }

// KnownHarnesses are the harnesses capability is reported for. Ordered so
// rendered output is stable.
var KnownHarnesses = []string{harness.DefaultName, harness.CodexName, harness.OpenCodeName}

// CapabilityFor reports what harnessName can do with an order requiring the
// given timing on the given trigger event.
//
// The honest position for v1:
//
//   - Claude Code and Codex CLI both expose a hook whose stdout reaches the
//     next model request inside the current turn, so both can meet
//     same-continuation on SessionStart.
//   - OpenCode's SSE projection is an observation path with no response
//     channel (see opencode_events.go — it calls ApplyHook and discards any
//     result), so it has no same-continuation channel at all. Its orders go
//     out on the message path, which is a queued turn.
//
// An unknown harness is reported as message-only rather than assumed capable.
// Guessing upward would mean promising a timing guarantee tclaude has never
// tested on that harness.
func CapabilityFor(timing, event, harnessName string) Capability {
	if event != db.StandingTriggerSessionStart {
		return Capability{
			Status:    StatusUnsupported,
			Transport: db.StandingTransportNone,
			Detail:    "unknown trigger event " + event,
		}
	}

	sameContinuation := harnessName == harness.DefaultName || harnessName == harness.CodexName

	switch timing {
	case db.StandingTimingSameContinuation:
		if sameContinuation {
			return Capability{
				Status:    StatusSupported,
				Transport: db.StandingTransportHookContext,
			}
		}
		return Capability{
			Status:    StatusUnsupported,
			Transport: db.StandingTransportNone,
			Detail: harnessName + " has no same-continuation context channel; " +
				"this order requires one, so nothing is delivered. " +
				"Re-author it with next-turn timing to reach this harness.",
		}

	case db.StandingTimingNextTurn:
		if sameContinuation {
			// Asking for next-turn and getting the stronger channel is not a
			// degradation — the requirement is met with room to spare.
			return Capability{
				Status:    StatusSupported,
				Transport: db.StandingTransportHookContext,
			}
		}
		return Capability{
			Status:    StatusSupported,
			Transport: db.StandingTransportMessage,
			Detail:    harnessName + " receives this as a queued turn, not inside the current one.",
		}
	}

	return Capability{
		Status:    StatusUnsupported,
		Transport: db.StandingTransportNone,
		Detail:    "unknown timing " + timing,
	}
}

// CapabilityByHarness reports capability for every known harness. The
// dashboard uses it to show, per order, which members of a mixed-harness group
// actually get what the operator asked for.
func CapabilityByHarness(timing, event string) map[string]Capability {
	out := make(map[string]Capability, len(KnownHarnesses))
	for _, h := range KnownHarnesses {
		out[h] = CapabilityFor(timing, event, h)
	}
	return out
}

// ReduceCapability reduces the answers for a specific set of harnesses to the
// worst case, so a single cell can say "not everyone gets this" without the
// operator having to expand every row.
//
// Worst-case rather than typical-case on purpose: the failure this feature has
// to avoid is an operator believing guidance reached agents it never reached.
//
// The caller supplies the harnesses that are ACTUALLY REACHABLE by the order —
// resolved from a single target agent's current generation, or from the live
// group roster for a group target. That distinction is the whole point of this
// function existing separately from PlatformCapability: rolling up across
// every harness tclaude knows about would mark a Claude-only single-agent order
// "unsupported" because some other agent somewhere runs OpenCode, which is not
// a fact about that order at all.
//
// An empty list reduces to unsupported-with-no-detail rather than to
// "supported": if nothing is reachable, nothing is delivered.
func ReduceCapability(timing, event string, harnesses []string) Capability {
	if len(harnesses) == 0 {
		return Capability{
			Status:    StatusUnsupported,
			Transport: db.StandingTransportNone,
			Detail:    "no reachable target",
		}
	}
	rank := map[Status]int{StatusSupported: 0, StatusDegraded: 1, StatusUnsupported: 2}
	worst := Capability{Status: StatusSupported, Transport: db.StandingTransportHookContext}
	var detail string
	for i, h := range harnesses {
		c := CapabilityFor(timing, event, h)
		if i == 0 || rank[c.Status] > rank[worst.Status] {
			worst = c
			detail = c.Detail
		} else if c.Status == worst.Status && c.Detail != "" && detail == "" {
			detail = c.Detail
		}
	}
	worst.Detail = detail
	return worst
}

// PlatformCapability is the worst case across every harness tclaude supports.
//
// It answers "could this order be authored to work everywhere", NOT "does this
// order reach its targets" — those are different questions and conflating them
// is misleading in both directions. Use ReduceCapability with the order's real
// target harnesses for the second one.
func PlatformCapability(timing, event string) Capability {
	return ReduceCapability(timing, event, KnownHarnesses)
}
