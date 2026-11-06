package agentd

import (
	"log/slog"
	"net/http"
	"os"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// startOpenCodeCloneServer readies a no-copy clone of a server-authoritative
// harness (OpenCode, tcl-ncywjf). Its conversation id is minted by the
// agentd-managed server its pane attaches to, so that server starts before
// the fork and its ses_ id becomes the clone's preset conv-id; the launch
// args are pointed at it.
//
// A clone is a new agent. Under tclaude's sandbox its server serves state
// private to that agent, so an actor id is reserved, given its own private
// state, and bound to the minted conversation, the way spawn does it.
//
// cleanup undoes all of it (server, private state, attachments directory,
// reserved actor) and is for a clone that did not come up.
func (r *cloneRun) startOpenCodeCloneServer(args *clcommon.SpawnArgs, label string) (
	launch *openCodeLaunch, cleanup func(), cerr *cloneSpawnError) {
	var steps []func()
	cleanup = func() {
		for i := len(steps) - 1; i >= 0; i-- {
			steps[i]()
		}
		steps = nil
	}
	fail := func(status int, code, msg string) (*openCodeLaunch, func(), *cloneSpawnError) {
		cleanup()
		return nil, func() {}, &cloneSpawnError{Status: status, Code: code, Msg: msg}
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(args.SandboxImplementation)
	if err != nil {
		return fail(http.StatusUnprocessableEntity, "unsupported_sandbox_profile_network", err.Error())
	}
	agentID := ""
	if implementation == sandboxpolicy.ImplementationTclaudeLayer {
		agentID = db.NewAgentID()
		discard, claimErr := claimPrivateOpenCodeState(agentID)
		if claimErr != nil {
			return fail(http.StatusInternalServerError, "spawn",
				"failed to allocate private OpenCode state: "+claimErr.Error())
		}
		if discard != nil {
			steps = append(steps, discard)
		}
	}
	created := false
	if err := prepareOpenCodeLaunchAttachments(args.SandboxImplementation, label, &created); err != nil {
		return fail(http.StatusInternalServerError, "spawn", err.Error())
	}
	if created {
		steps = append(steps, func() { _ = os.Remove(tclcommon.SpawnAttachmentsPrivateDir(label)) })
	}
	title := ""
	if isValidRenameTitle(r.p.Title) {
		title = r.p.Title
	}
	// A no-copy clone inherits its source's recorded posture, so it is a
	// sandbox continuation (args.SandboxContinuation) for its cgroup too.
	launch, err = startManagedOpenCodeServer(args, label, agentID, title, "", args.SandboxContinuation)
	if err != nil {
		return fail(http.StatusInternalServerError, "spawn",
			"failed to start managed OpenCode server: "+err.Error())
	}
	steps = append(steps, func() { _ = stopOpenCodeRuntime(launch.SessionID) })
	if agentID != "" {
		if _, _, err := db.EnsureAgentForConvWithID(launch.ConvID, agentID, "clone"); err != nil {
			return fail(http.StatusInternalServerError, "io", "bind clone identity: "+err.Error())
		}
		steps = append(steps, func() {
			if _, err := db.RetireAgentByID(agentID, "system:clone", "clone launch failed"); err != nil {
				slog.Warn("clone: retiring reserved actor failed", "agent", agentID, "error", err)
			}
		})
	}
	return launch, cleanup, nil
}
