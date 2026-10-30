package session

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// AgentResumeResolver maps an agent name or agent_id to the conversation of
// that agent's current generation. It returns "" with a nil error when the
// selector names no agent. The agent package owns selector resolution and
// imports this one, so it installs itself here.
var AgentResumeResolver func(selector string) (string, error)

// ResolveAgentResumeSelector is AgentResumeResolver, safe to call when the
// agent package isn't linked.
func ResolveAgentResumeSelector(selector string) (string, error) {
	if AgentResumeResolver == nil {
		return "", nil
	}
	return AgentResumeResolver(selector)
}

// expandAgentResumeSelector lets `--resume` take the name or agent_id that
// `tclaude agent ls` shows, by rewriting it to the conversation it stands for.
// A conversation match in the current scope wins, so existing ids keep meaning
// what they did and an old generation stays resumable by its own id. An agent
// is global, so a match also lifts the project scope; the launch then moves to
// the conversation's own directory as any other --resume does.
func expandAgentResumeSelector(params *NewParams) error {
	selector := strings.TrimSpace(params.Resume)
	if selector == "" || params.ManagedLaunch {
		return nil
	}
	h, err := harness.Resolve(strings.TrimSpace(params.Harness))
	if err != nil {
		return nil // runNew reports the unknown harness with its own message
	}
	cwd, err := resolveSessionDir(params.Dir)
	if err != nil {
		return nil // likewise for an unusable --dir
	}
	if !strings.HasPrefix(selector, db.AgentIDPrefix) {
		_, _, err := resolveResumeConv(h, resumeIDFromParam(h, selector), params.Global, cwd)
		if !errors.Is(err, errResumeNotFound) {
			return nil // resolved, or an error runNew reports
		}
	}
	convID, err := ResolveAgentResumeSelector(selector)
	if err != nil {
		return err
	}
	if convID == "" {
		return nil // runNew reports the miss for the selector as typed
	}
	if _, _, err := resolveResumeConv(h, convID, true, cwd); err != nil {
		return fmt.Errorf("agent %s has no %s conversation to resume (conversation %s): "+
			"it may have ended before its first message, or run on another harness (see --harness)",
			selector, h.Name, convID)
	}
	params.Resume = convID
	params.Global = true
	return nil
}
