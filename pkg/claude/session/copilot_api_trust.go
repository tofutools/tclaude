package session

import (
	"fmt"
	"os"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ValidateCopilotAPIFolderTrust refuses an API-driven Copilot launch into a
// directory Copilot does not trust yet and that this launch is not going to
// pre-trust.
//
// # Why this is a refusal and not a warning
//
// The folder-trust modal does NOT block the embedded server, and that is the
// whole reason this gate exists. Measured live against 1.0.78 with the pane
// parked on "Confirm folder trust": the port was listening, `connect`,
// `session.create`, `session.setForeground` and `session.send` all succeeded,
// and a real model turn completed with usage recorded — while the human's pane
// showed the modal for the entire turn.
//
// So an unattended API-drive spawn into an untrusted directory does not fail.
// It succeeds into a state where the agent is running and INVISIBLE: the pane —
// the surface a human looks at to decide whether an agent is alive — shows a
// blocking dialog about an agent that is answering prompts. tclaude's stated
// reason for driving Copilot this way rather than headlessly is that the human
// still sees and shares the pane, and that is exactly what this state removes.
// A launch whose success mode is a lying pane is worse than a named refusal.
//
// The modal cannot be cleared afterwards from the API side, which is what
// leaves refusal as the honest option. `session.permissions.folderTrust.
// addTrusted` does work — it needs a session of our own, since the pane's
// startup session is not drivable, and from one it reports success and flips
// `isTrusted` — but it does not retract a modal that is already drawn. It
// trusts the folder for the NEXT launch, which is what pre-seeding already does
// earlier and without the race.
//
// # Why it does not simply seed
//
// Pre-trusting edits a config file tclaude does not own, so it is deliberately
// never a default (see dir_trust.go). Making the API drive imply it would
// silently convert an opt-in about the transport into an opt-in about the
// operator's trust store. The launch says what it needs instead and lets the
// operator grant it — `--trust-dir`, the spawn-profile field, or the dashboard
// checkbox — which is the same consent that clears the modal for the send-keys
// drive.
//
// trustDir is the RESOLVED value, after the sibling-worktree auto-trust has had
// its say: a launch that is going to seed the directory a moment from now is
// not blocked here, because by the time the pane starts the entry will be
// there.
//
// # The resume path refuses too, and says something different
//
// resume marks a relaunch of an existing conversation — `session new -r`, which
// is the argv reincarnate and the dashboard's relaunch button fork. It changes
// only the wording, never the verdict: a relaunched agent whose directory lost
// its trust entry comes up just as invisible as a fresh one, and admitting it
// because it used to be trusted would be the proxy-value mistake again — a
// judgement about the launch that happened, applied to the launch happening
// now.
//
// What it does change is that the relaunch argv carries no --trust-dir and no
// profile, so a message leading with those hands the operator a remedy they
// cannot reach from where they are standing. Naming a flag the failing command
// cannot take is worse than naming none: it reads as "you forgot this", when
// what is true is that the trust entry disappeared out from under a
// conversation that had it.
//
// Silent for every non-Copilot harness and for every launch that did not ask
// for the API drive: this gates the channel, not the harness.
func ValidateCopilotAPIFolderTrust(
	h *harness.Harness,
	copilotAPI bool,
	trustDir bool,
	resume bool,
	cwd string,
	environment []sandboxpolicy.EnvironmentEntry,
) error {
	if !copilotAPI || trustDir {
		return nil
	}
	if h == nil || h.Name != harness.CopilotName {
		return nil
	}
	if strings.TrimSpace(cwd) == "" {
		// No directory to ask about YET. This is the spawn boundary's case: a
		// request that named no cwd is answered by resolveSessionDir inside
		// session.New, so refusing here would mean naming a directory nobody
		// has chosen — or guessing one, which is how a gate ends up confidently
		// refusing on the wrong path.
		//
		// Nothing is lost by waiting. The backstop call in session.New runs
		// with the RESOLVED cwd, so an unnamed directory is checked there
		// against the value the pane will actually use; what differs is only
		// where the operator sees the refusal.
		return nil
	}
	// The LAUNCH's environment, resolved exactly as the seeder resolves it, so
	// the file consulted here is the file the pane will open. Reading the
	// ambient COPILOT_HOME for a launch that relocates it would answer
	// confidently about the wrong file — and the answer would be "untrusted",
	// turning a perfectly good launch into a refusal naming a directory that is
	// trusted where it matters.
	launch := launchModelEnvironment(environment)
	home := strings.TrimSpace(launch["HOME"])
	if home == "" {
		resolved, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf(
				"resolve home directory for the Copilot API folder-trust check: %w", err)
		}
		home = resolved
	}
	trusted, err := harness.CopilotDirTrustedForLaunch(
		func(name string) string { return launch[name] }, home, cwd)
	if err != nil {
		return err
	}
	if trusted {
		return nil
	}
	// The remedies are named individually rather than as "opt into trust",
	// because they are reached from three different places and an operator who
	// is told only that the launch needs trust has to go and find out which one
	// applies to the surface they are standing in front of.
	remedy := "Pre-trust the directory — the spawn modal's pre-trust checkbox, a spawn " +
		"profile's trust_dir, or `tclaude session new --trust-dir` — or clear the dialog " +
		"once in a pane there, or spawn without the API drive"
	if resume {
		remedy = "This is a relaunch, so it carries no --trust-dir of its own: the entry was " +
			"there when the conversation was first launched and is not there now. Restore it " +
			"by clearing the dialog once in a pane there, or by launching once with " +
			"`tclaude session new --trust-dir` in that directory, then relaunch"
	}
	return fmt.Errorf(
		"the API-backed Copilot drive needs %s to be trusted before launch, and it is not "+
			"listed in %s. Copilot's folder-trust modal does not block the embedded server: "+
			"the agent would come up drivable over the API while its pane sat on the dialog, "+
			"so the human would be looking at a blocked pane for an agent that is running. %s",
		cwd, harness.DirTrustStore(h), remedy)
}
