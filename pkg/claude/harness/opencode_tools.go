package harness

import (
	"fmt"
	"slices"
	"strings"
)

const (
	// OpenCodeToolsAll allows every OpenCode tool, the ones tclaude has never
	// audited included: MCP and plugin tools, tools a later OpenCode adds, the
	// question tool, plan mode, and sub-agents (task). There is no deny-all
	// baseline. The per-tool entries below turn individual known tools off.
	OpenCodeToolsAll   = "all"
	OpenCodeToolsAllow = "allow"
	OpenCodeToolsAsk   = "ask"
	OpenCodeToolsDeny  = "deny"
)

// The per-tool entries `--tools all` accepts, as `all,<entry>=<allow|deny>`.
// Each names a tool (or file class) tclaude knows needs a deliberate choice
// when everything else is allowed.
const (
	// OpenCodeToolEntryQuestion is OpenCode's question tool, which stops and
	// waits for a human answer. Allowed by default: a detached agent that asks
	// shows as needing you, which is visible rather than unsafe.
	OpenCodeToolEntryQuestion = "question"
	// OpenCodeToolEntryPlan is plan mode (plan_enter and plan_exit). Allowed by
	// default.
	OpenCodeToolEntryPlan = "plan"
	// OpenCodeToolEntryEnv is reading .env files. Denied by default: the
	// ask OpenCode and tclaude otherwise use cannot reach a sub-agent, and
	// OpenCode's explore sub-agent would read them without asking.
	OpenCodeToolEntryEnv = "env"
)

// OpenCodeToolsPolicy is a parsed OpenCode tool-governance value. Only the
// all base carries entries.
type OpenCodeToolsPolicy struct {
	Base         string
	DenyQuestion bool
	DenyPlan     bool
	AllowEnv     bool
}

// AllTools reports the allow-everything base.
func (p OpenCodeToolsPolicy) AllTools() bool { return p.Base == OpenCodeToolsAll }

// String renders the canonical spelling: the base, then only the entries that
// differ from their default, in a fixed order. Profiles, the launch echo, and
// relaunch compare these strings, so one policy has exactly one spelling.
func (p OpenCodeToolsPolicy) String() string {
	parts := []string{p.Base}
	if p.DenyQuestion {
		parts = append(parts, OpenCodeToolEntryQuestion+"=deny")
	}
	if p.DenyPlan {
		parts = append(parts, OpenCodeToolEntryPlan+"=deny")
	}
	if p.AllowEnv {
		parts = append(parts, OpenCodeToolEntryEnv+"=allow")
	}
	return strings.Join(parts, ",")
}

// ParseOpenCodeToolGovernance parses `allow`, `ask`, `deny`, or
// `all[,question=allow|deny][,plan=allow|deny][,env=allow|deny]`.
func ParseOpenCodeToolGovernance(raw string) (OpenCodeToolsPolicy, error) {
	fields := strings.Split(strings.TrimSpace(raw), ",")
	policy := OpenCodeToolsPolicy{Base: strings.TrimSpace(fields[0])}
	switch policy.Base {
	case OpenCodeToolsAll, OpenCodeToolsAllow, OpenCodeToolsAsk, OpenCodeToolsDeny:
	default:
		return OpenCodeToolsPolicy{}, fmt.Errorf(
			"invalid opencode tool-governance policy %q (want %s|%s|%s|%s, and per-tool entries only after %s)",
			raw, OpenCodeToolsAll, OpenCodeToolsAllow, OpenCodeToolsAsk, OpenCodeToolsDeny, OpenCodeToolsAll)
	}
	seen := map[string]bool{}
	for _, field := range fields[1:] {
		field = strings.TrimSpace(field)
		if policy.Base != OpenCodeToolsAll {
			return OpenCodeToolsPolicy{}, fmt.Errorf(
				"opencode tool-governance entry %q needs the %q base (for example %q); %q already denies tools tclaude has not audited",
				field, OpenCodeToolsAll, "all,"+field, policy.Base)
		}
		name, action, ok := strings.Cut(field, "=")
		name, action = strings.TrimSpace(name), strings.TrimSpace(action)
		if !ok || (action != "allow" && action != "deny") {
			return OpenCodeToolsPolicy{}, fmt.Errorf(
				"opencode tool-governance entry %q must be <tool>=allow or <tool>=deny", field)
		}
		if seen[name] {
			return OpenCodeToolsPolicy{}, fmt.Errorf("opencode tool-governance entry %q is repeated", name)
		}
		seen[name] = true
		switch name {
		case OpenCodeToolEntryQuestion:
			policy.DenyQuestion = action == "deny"
		case OpenCodeToolEntryPlan:
			policy.DenyPlan = action == "deny"
		case OpenCodeToolEntryEnv:
			policy.AllowEnv = action == "allow"
		default:
			return OpenCodeToolsPolicy{}, fmt.Errorf(
				"unknown opencode tool-governance entry %q (want %s, %s, or %s)",
				name, OpenCodeToolEntryQuestion, OpenCodeToolEntryPlan, OpenCodeToolEntryEnv)
		}
	}
	return policy, nil
}

// ValidateOpenCodeToolsCombination refuses `--tools all` where tclaude cannot
// give OpenCode's sub-agents the same authority as the agent itself. A
// sub-agent inherits only the deny rules of the session policy, so:
//   - an "ask" rule (approval ask) would reach it as an allow, skipping the
//     human's OK;
//   - access-control's path rules rely on the deny-all baseline that all
//     drops, and their allows would not reach the sub-agent either.
func ValidateOpenCodeToolsCombination(harnessBuiltinMode, approval, tools string) error {
	policy, err := ParseOpenCodeToolGovernance(tools)
	if err != nil || !policy.AllTools() {
		return err
	}
	switch strings.TrimSpace(harnessBuiltinMode) {
	case OpenCodeSandboxTclaudeLayer, OpenCodeSandboxOff:
	default:
		return fmt.Errorf(
			"opencode --tools %s needs tclaude's sandbox (sandbox implementation tclaude-layer) or sandbox off: access-control's path rules depend on the deny-all baseline that %s removes",
			OpenCodeToolsAll, OpenCodeToolsAll)
	}
	if strings.TrimSpace(approval) == OpenCodeApprovalAsk {
		return fmt.Errorf(
			"opencode --tools %s cannot combine with approval %q: OpenCode sub-agents do not inherit ask rules, so they would act without your OK; use approval %q or %q, or a narrower --tools",
			OpenCodeToolsAll, OpenCodeApprovalAsk, OpenCodeApprovalDeny, OpenCodeApprovalAllowTools)
	}
	return nil
}

type openCodeToolGovernance struct{}

func (openCodeToolGovernance) DefaultPolicy() string { return OpenCodeToolsAllow }

func (openCodeToolGovernance) Modes() []string {
	return []string{OpenCodeToolsAll, OpenCodeToolsAllow, OpenCodeToolsAsk, OpenCodeToolsDeny}
}

func (openCodeToolGovernance) ModeHelp(policy string) string {
	switch strings.TrimSpace(policy) {
	case OpenCodeToolsAll:
		return "Allow every OpenCode tool, including MCP and plugin tools, ones tclaude has not audited, the question tool, plan mode, and sub-agents (task). Per-tool entries turn known ones off (question, plan) and .env reads on (env). Needs tclaude's sandbox or sandbox off, and approval deny or allow-tools."
	case OpenCodeToolsAllow:
		return "Allow bash, glob, grep, lsp, and skill in every sandbox mode; other tools stay denied. OpenCode sub-agents (task) are off; use all to turn them on."
	case OpenCodeToolsAsk:
		return "Ask before bash, glob, grep, lsp, or skill runs in every sandbox mode. OpenCode sub-agents (task) are off. ⚠ Detached agents can block waiting for a human response."
	case OpenCodeToolsDeny:
		return "Deny bash, glob, grep, lsp, and skill without prompting in every sandbox mode. OpenCode sub-agents (task) are off."
	default:
		return ""
	}
}

func (openCodeToolGovernance) ValidatePolicy(policy string) (string, error) {
	if strings.TrimSpace(policy) == "" {
		return "", nil
	}
	parsed, err := ParseOpenCodeToolGovernance(policy)
	if err != nil {
		return "", err
	}
	return parsed.String(), nil
}

// PolicyNotWider compares two OpenCode policies, entries included: policy is
// not wider than limit when its base is not wider and, between two all
// policies, it allows none of the entries limit denies.
func (g openCodeToolGovernance) PolicyNotWider(limit, policy string) bool {
	l, lerr := ParseOpenCodeToolGovernance(limit)
	p, perr := ParseOpenCodeToolGovernance(policy)
	if lerr != nil || perr != nil {
		return false
	}
	modes := g.Modes()
	at, bound := slices.Index(modes, p.Base), slices.Index(modes, l.Base)
	if at != bound {
		return at > bound
	}
	if !p.AllTools() {
		return true
	}
	return (p.DenyQuestion || !l.DenyQuestion) &&
		(p.DenyPlan || !l.DenyPlan) &&
		(!p.AllowEnv || l.AllowEnv)
}
