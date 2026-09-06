package harness

import "fmt"

// Claude Code's cross-session messaging, and why tclaude turns it off by
// default (TCL-812).
//
// Claude Code ships its own agent-to-agent mesh: a session binds an inbox
// socket, discovers your other sessions with the ListAgents tool, and delivers
// plain-text messages to them with SendMessage. Where the feature is present it
// is on with nothing to enable
// (https://code.claude.com/docs/en/cross-session-messaging).
//
// Under tclaude that mesh is a second, unmanaged coordination channel running
// alongside the one tclaude owns. Every property tclaude provides for agent
// messaging — group membership, permission slugs, the audit trail, the
// dashboard's view of who said what to whom — is absent from it. Two agents the
// operator deliberately put in different groups can still find each other and
// talk, and nothing in tclaude records that it happened. So tclaude closes the
// unmanaged channel by default and leaves `tclaude agent send` as the way
// agents coordinate, exactly as it disables Claude Code's auto-memory system in
// favour of its own state. An operator who wants the native mesh back opts in
// per profile, per spawn, or per session.
//
// WHAT "OFF" CAN AND CANNOT REACH. Claude Code splits this feature across three
// settings.json keys and deliberately gives it no single off switch — its own
// docs describe receiving and sending as separate controls. tclaude's OFF
// posture writes all three (see PeerMessagingSettings), which yields:
//
//   - Inbound is HARD off. crossSessionInbound=refuse drops every arriving peer
//     message without delivering it.
//   - Outbound is DISCOVERY off, not hard off. Denying ListAgents removes the
//     tool Claude uses to find peer sessions, but a peer name learned some other
//     way (realistically only the operator typing an @-mention) can still be
//     addressed.
//   - Cross-machine sends additionally require the operator's approval.
//
// The asymmetry is forced, not chosen: the deny rule for the send side would
// have to name SendMessage, which "takes the bare tool name with no specifier"
// — and per Claude Code's docs, "Denying SendMessage also removes messaging to
// subagents and agent-team teammates, since the same tool serves both". tclaude
// relies on in-harness subagents (the cold-review fallback in CLAUDE.md is
// one), so denying SendMessage would break a workflow the project depends on to
// close a hole that the fleet-wide default already closes from the other end:
// when every tclaude-launched agent refuses inbound, no tclaude agent can be
// REACHED by another regardless of what a sender attempts. What remains is a
// send toward a non-tclaude session the operator started by hand, and
// isolatePeerMachines covers that whenever it leaves the machine.
//
// A CAVEAT WORTH KEEPING, the same one context_features.go carries: these keys
// were read off Claude Code's published docs rather than a stable contract, and
// Claude Code is free to rename or retire them. A key that stops working
// degrades in the UNSAFE direction here — the agent regains a channel tclaude
// meant to close — which is the opposite of a stale context trim, so re-verify
// this file when Claude Code's messaging docs change shape. `/status` inside a
// spawned agent shows a `Peer address` row, and `/list-agents` is the cheap
// check for whether the deny landed.

const (
	// PeerMessagingInboundKey is Claude Code's inbound cross-session control.
	// tclaude writes it only in the refusing direction; see
	// PeerMessagingSettings for why the opt-in writes nothing.
	PeerMessagingInboundKey = "crossSessionInbound"
	// PeerMessagingInboundRefuse drops arriving peer messages without
	// delivering them to Claude. The other documented values are "accept" and
	// "hold"; tclaude never writes those.
	PeerMessagingInboundRefuse = "refuse"
	// PeerMessagingIsolateKey requires the operator's explicit approval before
	// any message reaches a session beyond this machine — even in
	// bypassPermissions mode, which skips ordinary permission prompts.
	PeerMessagingIsolateKey = "isolatePeerMachines"
	// PeerMessagingDenyTool is the tool whose denial removes peer DISCOVERY.
	// Deliberately not SendMessage: that tool also serves subagents and
	// agent-team teammates, and messaging a subagent never needs ListAgents
	// (Claude addresses it by the agent ID the Agent tool returned, or by a
	// name from the sibling roster), so denying this one leaves in-harness
	// subagent messaging fully intact.
	PeerMessagingDenyTool = "ListAgents"
)

// SupportsPeerMessaging reports whether the harness has a cross-session
// messaging system tclaude can steer. This is Claude Code's feature; Codex,
// OpenCode and Copilot expose no equivalent, so callers must not emit the
// settings for them — and must hide the affordance.
//
// Gated on the harness NAME rather than a capability func for the same reason
// SupportsAutoMemory and SupportsContextFeatures are: these are settings.json
// keys, not lifecycle commands with a per-harness implementation to probe.
func (h *Harness) SupportsPeerMessaging() bool {
	return h != nil && h.Name == DefaultName
}

// CanPeerMessaging is the UI-side predicate a spawn/profile control gates on
// (mirrors CanAutoMemory).
func (h *Harness) CanPeerMessaging() bool {
	return h.SupportsPeerMessaging()
}

// ResolvePeerMessaging gates the "let Claude Code keep its own cross-session
// messaging" opt-in and returns the posture to thread into the launch.
//
// Like ResolveAutoMemory there IS a meaningful non-zero default: tclaude
// recommends peer messaging OFF for the reasons at the top of this file, so an
// unset field (nil) resolves to false and the caller injects the refusal.
// Requesting peer messaging ON for a harness that has no such system is an
// error rather than a silent drop, so a mistake surfaces at the spawn boundary
// instead of at runtime.
//
// One function serves every spawn boundary: the daemon spawn path,
// `tclaude agent spawn`, `tclaude session new`, profile save and template
// deploy.
func ResolvePeerMessaging(h *Harness, requested *bool) (bool, error) {
	if requested == nil {
		return false, nil
	}
	if *requested && !h.CanPeerMessaging() {
		return false, fmt.Errorf("harness %q has no cross-session messaging system "+
			"(peer messaging is a Claude Code feature; not available for this harness)", harnessName(h))
	}
	return *requested, nil
}

// PeerMessagingSettings renders the settings.json keys and permission-deny
// rules a resolved posture contributes to the per-session `--settings` payload.
// A nil map and nil slice mean "inject nothing".
//
// Unlike AutoMemoryEnvValue and ContextFeatureEnv, this is deliberately
// ONE-DIRECTIONAL: OFF writes all three keys, ON writes nothing at all. Those
// siblings write an explicit value in both directions so a stray setting in the
// operator's own environment cannot override an agent that opted back in. That
// reasoning does not carry here, for a different reason per key:
//
//   - crossSessionInbound: with no value set, Claude Code does not simply
//     "accept" — it decides per message from the two sessions' permission
//     modes, holding a message for approval when the sending session bypasses
//     permission prompts and the receiver does not. That default is strictly
//     more careful than a blunt "accept", so an operator opting back in should
//     get it rather than have tclaude overwrite it.
//   - isolatePeerMachines: "A true from any settings scope applies, so a
//     checked-in project file can turn the requirement on but not off." Writing
//     false is therefore not merely unhelpful, it is inert.
//   - the deny rule: Claude Code evaluates deny before allow with no
//     specificity ordering, so there is no value that un-denies a tool another
//     scope denied.
//
// In other words `--peer-messaging` means "inject nothing and leave Claude Code
// and the operator's own settings in charge", not "force the feature on".
func PeerMessagingSettings(peerMessaging bool) (keys map[string]any, denyRules []string) {
	if peerMessaging {
		return nil, nil
	}
	return map[string]any{
		PeerMessagingInboundKey: PeerMessagingInboundRefuse,
		PeerMessagingIsolateKey: true,
	}, []string{PeerMessagingDenyTool}
}
